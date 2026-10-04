package replay

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"runtime"
	"time"
)

func (a *Service) obsLaunchRoutes(api *http.ServeMux) {
	api.HandleFunc("POST /api/obs/start", func(w http.ResponseWriter, r *http.Request) {
		if err := a.startHeadlessOBS(); err != nil {
			fail(w, err)
			return
		}
		respond(w, http.StatusAccepted, map[string]bool{"ok": true})
	})
}

func (a *Service) startHeadlessOBS() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if runtime.GOOS != "linux" || a.role != "agent" {
		return errors.New("请在 Linux 录制端启动无头 OBS")
	}
	if a.obsLaunch.Phase == "starting" || a.cs2Launch.Phase == "starting" || a.active || a.pendingJobsLocked() || a.localDemo.Phase == "armed" || a.localDemo.Phase == "running" {
		return errors.New("启动或录制任务正在进行，请等待完成后再启动 OBS")
	}
	lock := a.s.Config.SessionLock
	if lock == "" {
		lock = filepath.Join(a.dir, "obs-start.lock")
	}
	release, err := resourceLock(lock)
	if err != nil {
		return err
	}
	a.obsLaunch = CS2Launch{"starting", "正在无头显示器 :20 启动 OBS 并配置连接…"}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer release()
		ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
		defer cancel()
		url, password, err := launchHeadlessOBS(ctx)
		a.mu.Lock()
		defer a.mu.Unlock()
		if err == nil {
			a.s.Config.OBSURL, a.s.Config.OBSPassword = url, password
			a.saveLocked()
			if a.storageErr != "" {
				err = errors.New("OBS 已启动，但保存连接配置失败：" + a.storageErr)
			}
		}
		if err != nil {
			a.obsLaunch = CS2Launch{"failed", err.Error()}
			a.logLocked("warn", "无头 OBS："+err.Error())
		} else {
			a.obsLaunch = CS2Launch{"ready", "OBS 已在无头显示器 :20 就绪，连接配置已保存；可安装战队 HUD"}
			a.logLocked("info", a.obsLaunch.Message)
		}
	}()
	return nil
}
