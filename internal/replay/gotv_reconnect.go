package replay

import (
	"errors"
)

// Accept first, then let recording cleanup release the camera without holding
// mu. GSI ingestion and OBS stop acknowledgement must continue during this wait.
func (a *Service) beginGOTVReconnectLocked(c Config) {
	a.s.Config.Epoch++
	a.s.Config.Mode = "live"
	a.resetDemoTimelineLocked()
	a.localDemo = LocalDemo{}
	a.refreshPlayerMappingLocked(true)
	a.s.Config.GOTV = c.GOTV
	a.s.Config.GOTVPassword = c.GOTVPassword
	for i := range a.s.Jobs {
		j := &a.s.Jobs[i]
		if j.Status == "COMMITTED" || j.Status == "CAPTURING" || j.Status == "STOPPING" {
			j.Status = "CANCELLED"
			j.Error = "GOTV 重连：旧录制任务已取消"
		}
	}
	for i := range a.s.Events {
		e := &a.s.Events[i]
		if !terminal(e.Status) {
			e.Status = "CANCELLED"
			e.Reason = "GOTV 重连：旧时间线已失效"
		}
	}
	a.gotvReconnect = CS2Launch{Phase: "starting", Message: "正在结束旧录制 / 自动映射并重连 GOTV"}
	a.saveLocked()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		err := a.reconnectGOTV(c)
		a.mu.Lock()
		defer a.mu.Unlock()
		if err != nil {
			a.gotvReconnect = CS2Launch{Phase: "failed", Message: "重连未完成：" + err.Error() + "；请检查 B 路 CS2 和控制台连接后重试"}
		} else {
			// Drop packets received while the previous game was shutting down.
			a.resetDemoTimelineLocked()
			a.refreshPlayerMappingLocked(true)
			a.s.Config.Paused = !a.s.Config.AutoCapture
			a.gotvReconnect = CS2Launch{Phase: "sent", Message: "GOTV 重连命令已发送，等待新的比赛 GSI"}
			a.gotvSentAt = nowMS()
		}
		a.active = a.pendingJobsLocked()
		a.logLocked("info", a.gotvReconnect.Message)
		a.saveLocked()
	}()
}

func (a *Service) reconnectGOTV(c Config) error {
	deadline := nowMS() + 30000
	for !a.captureMu.TryLock() {
		if nowMS() >= deadline {
			return errors.New("等待 OBS 停止或相机释放超时")
		}
		if err := a.waitUntil(nowMS() + 40); err != nil {
			return err
		}
	}
	defer a.captureMu.Unlock()
	for {
		if a.ctx.Err() != nil {
			return a.ctx.Err()
		}
		a.mu.Lock()
		launching := a.cs2Launch.Phase == "starting"
		a.mu.Unlock()
		if !launching {
			break
		}
		if nowMS() >= deadline {
			return errors.New("等待 CS2 启动超时")
		}
		if err := a.waitUntil(nowMS() + 100); err != nil {
			return err
		}
	}
	release, err := resourceLock(c.SessionLock)
	if err != nil {
		return err
	}
	defer release()
	// Always disconnect even for the same address; always set/clear password first.
	command := "disconnect; spec_show_xray 1; password \"" + c.GOTVPassword + "\"; connect " + c.GOTV
	if err := netCommand(c.NetCon, command); err != nil {
		return err
	}
	return nil
}
