package replay

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
)

func (a *Service) closeCS2() ([]int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.role != "agent" {
		return nil, errors.New("请在 Linux Agent 上关闭本机 CS2")
	}
	if a.active || a.cs2Launch.Phase == "starting" {
		return nil, errors.New("Replay 正在启动游戏、录制或处理任务，请等待结束后关闭 CS2")
	}
	lock := a.s.Config.SessionLock
	if lock == "" {
		lock = filepath.Join(a.dir, "cs2-close.lock")
	}
	release, err := resourceLock(lock)
	if err != nil {
		return nil, err
	}
	defer release()
	// Cancel the timer before killing: a failed kill must not leave a pending resume.
	a.cs2Launch = CS2Launch{}
	a.localDemo = LocalDemo{Phase: "stopped"}
	a.resetDemoTimelineLocked()
	pids, err := terminateCS2()
	if err != nil {
		a.localDemo.Error = err.Error()
		a.logLocked("warn", "关闭 CS2："+err.Error())
	} else {
		a.logLocked("info", fmt.Sprintf("已关闭本机 CS2，共 %d 个进程；Demo 启动已取消，采集已暂停", len(pids)))
	}
	a.saveLocked()
	return pids, err
}
func (a *Service) cs2Routes(api *http.ServeMux) {
	api.HandleFunc("POST /api/cs2/start", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			MoveSteam bool `json:"move_steam"`
		}
		if err := decode(w, r, &input); err != nil {
			fail(w, err)
			return
		}
		if err := a.startHeadlessCS2WithMove(input.MoveSteam); err != nil {
			fail(w, err)
			return
		}
		respond(w, 202, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/cs2/close", func(w http.ResponseWriter, r *http.Request) {
		pids, err := a.closeCS2()
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, 200, map[string]any{"ok": true, "closed": len(pids)})
	})
}
