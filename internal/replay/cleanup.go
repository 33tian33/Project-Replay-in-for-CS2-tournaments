package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Only raw files returned by Replay's own StopRecord calls are eligible.
func (a *Service) rememberRaw(path string) {
	if path == "" || !filepath.IsAbs(path) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ledger := filepath.Join(a.dir, "raw-recordings.json")
	var paths []string
	if b, err := os.ReadFile(ledger); err == nil {
		if json.Unmarshal(b, &paths) != nil {
			a.logLocked("warn", "原始录制索引损坏，未覆盖")
			return
		}
	} else if !os.IsNotExist(err) {
		a.logLocked("warn", err.Error())
		return
	}
	for _, p := range paths {
		if p == path {
			return
		}
	}
	if err := saveJSON(ledger, append(paths, path)); err != nil {
		a.logLocked("warn", "保存原始录制索引失败："+err.Error())
	}
}
func (a *Service) cleanupRoutes(api *http.ServeMux) {
	api.HandleFunc("POST /api/cleanup", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("agent") == "1" && a.role != "agent" {
			fail(w, errors.New("同步目标必须是 Linux Agent"))
			return
		}
		a.maintenance.Lock()
		defer a.maintenance.Unlock()
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.active || a.pendingJobsLocked() || a.cs2Launch.Phase == "starting" || a.localDemo.Phase == "armed" || a.localDemo.Phase == "running" || a.playback != "" || a.output.ID != "" {
			fail(w, errors.New("请先停止 Demo / 回放，等待录制和回传完成后清理"))
			return
		}
		remote := false
		if a.role == "director" && remoteConfigured(a.s.Config) {
			// Fail before local deletion when the peer cannot be cleaned. Retrying is safe.
			if err := remoteCall(r.Context(), a.s.Config, "POST", "/api/cleanup?agent=1", struct{}{}, nil); err != nil {
				fail(w, fmt.Errorf("Linux 清理未完成，本机尚未清理：%w", err))
				return
			}
			remote = true
		}
		if err := a.cleanupLocked(); err != nil {
			fail(w, fmt.Errorf("清理未全部完成（Linux 已清理=%t），可重试：%w", remote, err))
			return
		}
		respond(w, 200, map[string]bool{"ok": true, "remote_cleaned": remote})
	})
}
func (a *Service) cleanupLocked() error {
	ledger := filepath.Join(a.dir, "raw-recordings.json")
	var raw []string
	if b, err := os.ReadFile(ledger); err == nil {
		if err = json.Unmarshal(b, &raw); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, path := range raw {
		ext := strings.ToLower(filepath.Ext(path))
		if !filepath.IsAbs(path) || (ext != ".mkv" && ext != ".mp4" && ext != ".mov" && ext != ".flv" && ext != ".ts" && ext != ".avi") {
			return fmt.Errorf("原始录制索引含非视频路径")
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("原始录制路径不是普通文件")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	media := filepath.Join(a.dir, "media")
	info, err := os.Lstat(media)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("media 必须为数据目录内的真实文件夹")
	}
	entries, err := os.ReadDir(media)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == a.s.Output.Transition1 || entry.Name() == a.s.Output.Transition2 {
			continue
		}
		if err = os.RemoveAll(filepath.Join(media, entry.Name())); err != nil {
			return err
		}
	}
	entries, err = os.ReadDir(a.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "raw-recordings.json" || strings.HasPrefix(name, ".state-") || name == "tmp" || name == "downloads" || name == "logs" {
			if err = os.RemoveAll(filepath.Join(a.dir, name)); err != nil {
				return err
			}
		}
	}
	c, out := a.s.Config, a.s.Output
	a.s = State{Config: c, Output: out, HighlightsVersion: 1, Events: []Event{}, Jobs: []Job{}, Artifacts: []Artifact{}, Queue: []string{}, HalfQueue: []string{}, Logs: []Log{}}
	a.output = OutputSession{}
	a.localDemo = LocalDemo{}
	a.endRound = 0
	a.manualRounds = nil
	a.halfTriggered = false
	a.fullTriggered = false
	a.previous = map[string]map[string]any{}
	a.roundClocks = nil
	a.phaseAnchors = nil
	a.liveDuration = nil
	a.gsiSeen = map[string]int64{}
	a.gsiRound = map[string]int{}
	a.gsiMap = map[string]string{}
	a.gsiDiagnostics = nil
	a.observed = ""
	a.observedAt = 0
	a.remoteAt = 0
	a.saveLocked()
	if a.storageErr != "" {
		return errors.New(a.storageErr)
	}
	return nil
}
