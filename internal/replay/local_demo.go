package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// LocalDemo is ephemeral: restarting the service never resumes a game.
type LocalDemo struct {
	Phase  string  `json:"phase"`
	Path   string  `json:"path"`
	SHA256 string  `json:"sha256"`
	Tick   int64   `json:"tick"`
	RunID  string  `json:"run_id"`
	Due    int64   `json:"due"`
	SentAt int64   `json:"sent_at"`
	Delay  float64 `json:"delay"`
	Error  string  `json:"error"`
}
type demoRequest struct {
	Action string  `json:"action"`
	Path   string  `json:"path"`
	Tick   int64   `json:"tick"`
	Delay  float64 `json:"delay"`
	RunID  string  `json:"run_id"`
	Due    int64   `json:"due"`
	SHA256 string  `json:"sha256"`
}

var demoFilePattern = regexp.MustCompile(`File:([^\r\n]+)`)

var demoTickPattern = regexp.MustCompile(`Currently playing (\d+) of (\d+) ticks`)

func demoPath(path string) (string, error) {
	path = strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	if !filepath.IsAbs(path) || len(path) == 0 || len(path) > 1024 || !strings.HasSuffix(strings.ToLower(path), ".dem") || strings.ContainsAny(path, "\";\r\n\x00") {
		return "", errors.New("请输入本机 .dem 文件绝对路径，不得含引号、分号或换行")
	}
	return path, nil
}
func demoHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("Demo 必须是普通文件")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (a *Service) demoCommandLocked(command string, feedback bool) (string, error) {
	if a.role == "agent" {
		release, err := resourceLock(a.s.Config.SessionLock)
		if err != nil {
			return "", err
		}
		defer release()
	}
	if !feedback {
		return "", netCommand(a.s.Config.NetCon, command)
	}
	n, err := openConsole(a.s.Config.NetCon)
	if err != nil {
		return "", err
	}
	defer n.conn.Close()
	return n.query(command)
}
func (a *Service) demoAction(p demoRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active || a.cs2Launch.Phase == "starting" {
		return errors.New("录制已锁定，请等待完成再操作本地 Demo")
	}
	if a.s.Config.Mode != "live" {
		return errors.New("本地 .dem 联调使用真实模式 live；内置演示模式不控制游戏")
	}
	if p.Action != "stop" && (a.localDemo.Phase == "armed" || a.localDemo.Phase == "running") {
		return errors.New("请先停止当前 Demo 联调")
	}
	switch p.Action {
	case "load":
		path, err := demoPath(p.Path)
		if err != nil {
			return err
		}
		hash, err := demoHash(path)
		if err != nil {
			return err
		}
		if _, err = a.demoCommandLocked(`cl_demo_predict 0; playdemo "`+path+`"`, false); err != nil {
			return err
		}
		a.localDemo = LocalDemo{Phase: "loaded", Path: path, SHA256: hash}
		a.s.Queue = nil
		a.s.HalfQueue = nil
		a.s.FullQueue = nil
		a.resetDemoTimelineLocked()
	case "seek":
		if a.localDemo.Path == "" || p.Tick < 0 || p.Tick > 100000000 {
			return errors.New("先载入 Demo，再指定有效起始 tick")
		}
		if _, err := a.demoCommandLocked(fmt.Sprintf("demo_timescale 1; demo_pause; demo_gototick %d 0 1", p.Tick), false); err != nil {
			return err
		}
		a.localDemo.Phase = "seeking"
		a.localDemo.Tick = p.Tick
		a.localDemo.Error = ""
		a.resetDemoTimelineLocked()
	case "ready":
		if a.localDemo.Phase != "seeking" && a.localDemo.Phase != "ready" {
			return errors.New("先定位并暂停，再检查就绪")
		}
		out, err := a.demoCommandLocked("demo_pause; demo_gototick", true)
		if err != nil {
			return err
		}
		m := demoTickPattern.FindStringSubmatch(out)
		if m == nil {
			return errors.New("未读到 Demo tick，等待 CS2 载入完成后重试")
		}
		tick, _ := strconv.ParseInt(m[1], 10, 64)
		total, _ := strconv.ParseInt(m[2], 10, 64)
		if tick != a.localDemo.Tick || tick >= total {
			return fmt.Errorf("Demo 当前 tick %d，目标 %d；请等待定位完成或重新定位", tick, a.localDemo.Tick)
		}
		fileMatch := demoFilePattern.FindStringSubmatch(out)
		if fileMatch == nil {
			return errors.New("CS2 未返回当前 Demo 文件路径，不能确认载入成功")
		}
		actual := filepath.Clean(strings.ReplaceAll(strings.TrimSpace(fileMatch[1]), "\\", "/"))
		expected := filepath.Clean(a.localDemo.Path)
		same := actual == expected
		if runtime.GOOS == "windows" {
			same = strings.EqualFold(actual, expected)
		}
		if !same {
			return fmt.Errorf("CS2 当前 Demo 不是所选文件：%s；请重新载入", actual)
		}
		a.localDemo.Phase = "ready"
	case "arm":
		if a.localDemo.Phase != "ready" || a.localDemo.SHA256 != p.SHA256 || a.localDemo.Tick != p.Tick || !validID(p.RunID) {
			return errors.New("两端必须就绪、文件 SHA-256 和起始 tick 相同")
		}
		if p.Due < nowMS()+500 || p.Due > nowMS()+310000 || math.IsNaN(p.Delay) || math.IsInf(p.Delay, 0) || p.Delay < 0 || p.Delay > 300 {
			return errors.New("启动时刻或延迟无效")
		}
		a.localDemo.Phase = "armed"
		a.localDemo.RunID = p.RunID
		a.localDemo.Due = p.Due
		a.localDemo.Delay = p.Delay
		a.localDemo.SentAt = 0
		a.resetDemoTimelineLocked()
		a.s.Config.Delta = p.Delay
	case "stop":
		if p.RunID != "" && a.localDemo.RunID != p.RunID {
			return errors.New("测试会话已改变，拒绝停止其他会话")
		}
		if a.localDemo.Phase == "" {
			return nil
		}
		// Disarm first: even a broken NetCon must never leave a delayed resume queued.
		a.localDemo.Phase = "stopped"
		a.localDemo.Due = 0
		a.resetDemoTimelineLocked()
		if _, err := a.demoCommandLocked("demo_pause", false); err != nil {
			a.localDemo.Error = err.Error()
			a.saveLocked()
			return err
		}
	default:
		return errors.New("未知 Demo 操作")
	}
	a.logLocked("info", "本地 Demo 联调："+p.Action+"；启动时间差不是实测校准，核验后再启用采集")
	a.saveLocked()
	return nil
}
func (a *Service) resetDemoTimelineLocked() {
	a.s.Config.Paused = true
	a.s.Config.CalibratedUntil = 0
	a.previous = map[string]map[string]any{}
	a.gsiSeen = map[string]int64{}
	a.gsiMap = map[string]string{}
	a.gsiRound = map[string]int{}
	a.observed = ""
	a.observedAt = 0
	a.roundClocks = nil
	a.liveDuration = nil
	a.output = OutputSession{}
	a.endRound = 0
	a.halfTriggered = false
	a.fullTriggered = false
	// Old unsubmitted candidates must not enter a subsequent test run.
	for i := range a.s.Events {
		if a.s.Events[i].JobID == "" {
			a.s.Events[i].Status = "CANCELLED"
			a.s.Events[i].Epoch = 0
		}
	}
}
func (a *Service) tickDemoLocked() {
	if a.localDemo.Phase != "armed" || nowMS() < a.localDemo.Due {
		return
	}
	if a.active || nowMS()-a.localDemo.Due > 500 {
		a.localDemo.Phase = "failed"
		a.localDemo.Error = "错过启动时刻或录制资源忙，请停止两端并重试"
		a.saveLocked()
		return
	}
	if _, err := a.demoCommandLocked("demo_timescale 1; demo_resume; "+a.recordingHUDCommandLocked(), false); err != nil {
		a.localDemo.Phase = "failed"
		a.localDemo.Error = err.Error()
	} else {
		a.localDemo.Phase = "running"
		a.localDemo.SentAt = nowMS()
		a.s.Config.Paused = !a.s.Config.AutoCapture
		a.logLocked("info", fmt.Sprintf("本地 Demo 已发送播放命令，计划 %d，发送完成 %d；请实测 A/B 差值", a.localDemo.Due, a.localDemo.SentAt))
	}
	a.saveLocked()
}
func (a *Service) startDemoPair(delay float64) error {
	if math.IsNaN(delay) || math.IsInf(delay, 0) || delay < 0 || delay > 300 {
		return errors.New("B 路延迟必须在 0–300 秒之间")
	}
	if a.role != "director" {
		return errors.New("请在 Windows 导播端启动双路测试")
	}
	if err := a.syncRemote(); err != nil {
		return err
	}
	a.mu.Lock()
	c, d, offset := a.s.Config, a.localDemo, a.remoteOffset
	a.mu.Unlock()
	if c.Mode != "live" || d.Phase != "ready" {
		return errors.New("A 路必须处于真实模式且 Demo 已就绪")
	}
	var remote struct {
		Demo  LocalDemo `json:"local_demo"`
		State State     `json:"state"`
	}
	if err := remoteCall(a.ctx, c, "GET", "/api/state", nil, &remote); err != nil {
		return err
	}
	if remote.State.Config.Mode != "live" || remote.Demo.Phase != "ready" || remote.Demo.SHA256 != d.SHA256 || remote.Demo.Tick != d.Tick {
		return errors.New("B 路未就绪，或两端 Demo 文件 / 起始 tick 不一致")
	}
	run := id()
	due := nowMS() + 5000
	p := demoRequest{Action: "arm", RunID: run, Due: due + offset + int64(delay*1000), SHA256: d.SHA256, Tick: d.Tick, Delay: delay}
	if err := remoteCall(a.ctx, c, "POST", "/api/local-demo", p, nil); err != nil {
		cleanup := remoteCall(a.ctx, c, "POST", "/api/local-demo", demoRequest{Action: "stop", RunID: run}, nil)
		return fmt.Errorf("B 路启动结果不确定：%v；撤销结果：%v。请核对 B 路状态", err, cleanup)
	}
	p.Due = due
	if err := a.demoAction(p); err != nil {
		cleanup := remoteCall(a.ctx, c, "POST", "/api/local-demo", demoRequest{Action: "stop", RunID: run}, nil)
		return fmt.Errorf("A 路启动失败：%v；B 路撤销结果：%v", err, cleanup)
	}
	return nil
}
func (a *Service) demoRoutes(api *http.ServeMux) {
	api.HandleFunc("POST /api/local-demo", func(w http.ResponseWriter, r *http.Request) {
		var p demoRequest
		if err := decode(w, r, &p); err != nil {
			fail(w, err)
			return
		}
		var err error
		switch p.Action {
		case "start-pair":
			err = a.startDemoPair(p.Delay)
		case "stop-pair":
			if a.role != "director" {
				err = errors.New("请在导播端停止双路测试")
				break
			}
			a.mu.Lock()
			c, run := a.s.Config, a.localDemo.RunID
			a.mu.Unlock()
			if run == "" {
				err = errors.New("没有双路测试会话，请分别停止本机 Demo")
				break
			}
			e1 := a.demoAction(demoRequest{Action: "stop", RunID: run})
			e2 := remoteCall(a.ctx, c, "POST", "/api/local-demo", demoRequest{Action: "stop", RunID: run}, nil)
			err = errors.Join(e1, e2)
		default:
			err = a.demoAction(p)
		}
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
}
