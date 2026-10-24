package replay

import (
	"runtime"
	"time"
)

// Start the delay on the first accepted match GSI, not on the connect write:
// loading the server can take longer than five seconds.
func (a *Service) scheduleGOTVHUDLocked() {
	if runtime.GOOS != "linux" || a.role != "agent" || a.s.Config.Mode != "live" {
		return
	}
	c, sentAt := a.s.Config, a.gotvSentAt
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-a.ctx.Done():
			return
		case <-timer.C:
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		// Serialize with new connection requests, and discard obsolete timers.
		if a.ctx.Err() != nil || a.gotvSentAt != sentAt || a.gotvReconnect.Phase != "receiving" || a.s.Config.Epoch != c.Epoch || a.s.Config.NetCon != c.NetCon || a.s.Config.Mode != "live" || nowMS()-a.gsiSeen["b"] > 5000 || a.observed == "" {
			return
		}
		if err := netCommand(c.NetCon, teamHUDCommand); err != nil {
			a.logLocked("warn", "GOTV 连接后延迟 5 秒隐藏原生 UI：发送失败："+err.Error())
			return
		}
		a.nativeHUDHidden = true
		a.logLocked("info", "GOTV 连接后延迟 5 秒：已发送隐藏原生 UI 指令，保留准星与 X 光")
	}()
}
