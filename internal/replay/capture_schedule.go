package replay

import (
	"errors"
	"net/http"
)

// Attach late observations to the same still-open camera recording. Never extend
// across a reservation already promised to a different player.
func (a *Service) extendCapturesLocked(c Config, now int64) {
	if c.Paused {
		return
	}
	for i := range a.s.Events {
		e := &a.s.Events[i]
		if e.JobID != "" || e.Time == 0 || e.Utility != nil || e.Epoch != c.Epoch || e.Match != c.Match || e.Map != c.Map || e.Uncertainty > c.Guard {
			continue
		}
		start, end := bounds(*e, c)
		for k := range a.s.Jobs {
			j := &a.s.Jobs[k]
			if j.Status != "COMMITTED" && j.Status != "CAPTURING" {
				continue
			}
			if len(j.Events) == 0 || j.Epoch != c.Epoch || !sameTarget(j.Events[0], *e) || e.Round != j.Events[0].Round || start < j.Start || start > j.End || now >= j.End || end-j.Start > 30000 {
				continue
			}
			nextEnd := max(j.End, end)
			conflict := false
			for _, other := range a.s.Jobs {
				if other.ID != j.ID && reservesCamera(other.Status) && other.Start >= j.Start && other.Start < nextEnd+int64((c.Setup+c.Transition)*1000) {
					conflict = true
					break
				}
			}
			if conflict {
				continue
			}
			e.JobID = j.ID
			e.Status = j.Status
			e.Reason = "同一选手连续击杀，复用当前录制"
			j.Events = append(j.Events, jsonCopy(*e))
			j.End = nextEnd
			a.saveLocked()
			break
		}
	}
}
func (a *Service) captureSnapshot(j Job, stopIfElapsed bool) Job {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.s.Jobs {
		if a.s.Jobs[i].ID == j.ID {
			if stopIfElapsed && nowMS() >= a.s.Jobs[i].End {
				a.s.Jobs[i].Status = "STOPPING"
			}
			return jsonCopy(a.s.Jobs[i])
		}
	}
	if stopIfElapsed && nowMS() >= j.End {
		j.Status = "STOPPING"
	}
	return j
}
func (a *Service) timingRoutes(api *http.ServeMux) {
	api.HandleFunc("POST /api/timing/compact", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("agent") == "1" && a.role != "agent" {
			fail(w, errors.New("同步目标必须是 Linux Agent"))
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.active || a.pendingJobsLocked() {
			fail(w, errors.New("等待当前录制 / 回传完成后再调整间隔"))
			return
		}
		if a.role == "director" && remoteConfigured(a.s.Config) {
			if err := remoteCall(r.Context(), a.s.Config, "POST", "/api/timing/compact?agent=1", struct{}{}, nil); err != nil {
				fail(w, err)
				return
			}
		}
		a.s.Config.Guard = max(.2, a.s.Config.Uncertainty+.05)
		a.s.Config.Setup = .2
		a.s.Config.Transition = 0
		a.saveLocked()
		if a.storageErr != "" {
			fail(w, errors.New(a.storageErr))
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
}
