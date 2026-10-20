package replay

import "time"

// StartPresentation is called by the application after its HTTP port is known.
// Retries let OBS start later without requiring another click in the UI.
func (a *Service) StartPresentation(host string) {
	if a.role != "agent" {
		return
	}
	a.startHeadlessWindowGuard()
	a.startDefaultHUD(host)
}

func (a *Service) startDefaultHUD(host string) {
	a.mu.Lock()
	a.presentationHost = host
	a.startupHUDPending = true
	a.mu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		lastError := ""
		for {
			a.mu.Lock()
			if !a.startupHUDPending || a.s.HUD.Mode != "astra" {
				a.startupHUDPending = false
				a.mu.Unlock()
				return
			}
			if !a.active && !a.pendingJobsLocked() && a.obsLaunch.Phase != "starting" && a.cs2Launch.Phase != "starting" && a.localDemo.Phase != "armed" && a.localDemo.Phase != "running" {
				_, err := a.applyHUDLocked(host)
				if err != nil && err.Error() != lastError {
					lastError = err.Error()
					a.logLocked("warn", "等待自动应用 Astra HUD："+lastError)
				}
			}
			done := !a.startupHUDPending
			a.mu.Unlock()
			if done {
				return
			}
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
