package replay

import (
	"errors"
	"net/http"
	"sort"
)

const cameraBeforeMS int64 = 250
const cameraAfterMS int64 = 150

func switchSetupMS(c Config) int64 { return max(200, int64(c.Setup*1000)) }

type cameraShot struct {
	Player             string
	Switch, Ready, End int64
	Events             []Event
}

// Preserve full handles where possible, trimming only at direct camera cuts.
// Planning, live switching and export must use this exact same timeline.
func cameraShots(j Job, c Config) []cameraShot {
	if len(j.Events) == 0 {
		return nil
	}
	shots := []cameraShot{{Player: j.Events[0].Player, Switch: j.Start, Ready: j.Start, End: j.End, Events: []Event{j.Events[0]}}}
	guard := int64(c.Guard * 1000)
	for i, e := range j.Events[1:] {
		last := &shots[len(shots)-1]
		if sameTarget(j.Events[i], e) {
			last.Events = append(last.Events, e)
			continue
		}
		cut := min(j.Events[i].Time+replayAfterMS+guard, e.Time-cameraBeforeMS-guard-switchSetupMS(c))
		cut = max(cut, e.Time-replayBeforeMS-guard-switchSetupMS(c))
		last.End = cut
		shots = append(shots, cameraShot{Player: e.Player, Switch: cut, Ready: cut + switchSetupMS(c), End: j.End, Events: []Event{e}})
	}
	return shots
}

func cameraJob(j Job, c Config, at int64) (Job, int64) {
	shots := cameraShots(j, c)
	if len(shots) == 0 {
		return j, j.Start
	}
	shot := shots[0]
	for _, next := range shots[1:] {
		if at < next.Switch {
			break
		}
		shot = next
	}
	j.Player, j.Start, j.End, j.Events = shot.Player, shot.Ready, shot.End, shot.Events
	j.Slot = c.Mappings[j.Player]
	return j, shot.Switch
}

func eventClipBounds(j Job, e Event, c Config) (int64, int64) {
	start, end := e.Time-replayBeforeMS, e.Time+replayAfterMS
	for _, shot := range cameraShots(j, c) {
		for _, candidate := range shot.Events {
			if candidate.ID == e.ID {
				return max(start, shot.Ready+int64(c.Guard*1000)), min(end, shot.End-int64(c.Guard*1000))
			}
		}
	}
	return start, end
}

// Append feasible late observations in planner priority order, without rewriting
// a camera cut that has already happened or stealing another reservation.
func (a *Service) extendCapturesLocked(c Config, now int64) {
	if c.Paused {
		return
	}
	indices := make([]int, len(a.s.Events))
	for i := range indices {
		indices[i] = i
	}
	sort.Slice(indices, func(i, j int) bool {
		x, y := a.s.Events[indices[i]], a.s.Events[indices[j]]
		if x.Time != y.Time {
			return x.Time < y.Time
		}
		return x.ID < y.ID
	})
	for _, i := range indices {
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
			if len(j.Events) == 0 || j.Utility != nil || j.Epoch != c.Epoch || e.Round != j.Events[0].Round || start < j.Start || start > j.End+1500 || now >= j.End || end-j.Start > 30000 {
				continue
			}
			last := j.Events[len(j.Events)-1]
			if e.Time < last.Time || !compatible(last, *e, c) {
				continue
			}
			// Select among feasible extensions only. Their pre-roll may already be
			// recorded, so the standalone full-window deadline does not apply.
			var candidates []Event
			planNow := now
			for _, candidate := range a.s.Events {
				if candidate.JobID != "" || candidate.Utility != nil || candidate.Round != e.Round || candidate.Time < last.Time || !compatible(last, candidate, c) {
					continue
				}
				cs, ce := bounds(candidate, c)
				if cs < j.Start || cs > j.End+1500 || ce-j.Start > 30000 || candidate.Time <= now {
					continue
				}
				proposed := *j
				proposed.Events = append(append([]Event(nil), j.Events...), candidate)
				proposed.End = max(j.End, ce)
				shots := cameraShots(proposed, c)
				if !sameTarget(last, candidate) && shots[len(shots)-1].Switch <= now+50 {
					continue
				}
				candidates = append(candidates, candidate)
				planNow = min(planNow, cs-1)
			}
			var others []Job
			for _, other := range a.s.Jobs {
				if other.ID != j.ID {
					others = append(others, other)
				}
			}
			selected := false
			for _, candidate := range Plan(candidates, others, c, planNow) {
				if candidate.ID == e.ID && candidate.Status == "SCHEDULED" {
					selected = true
				}
			}
			if !selected {
				continue
			}
			proposed := *j
			proposed.Events = append(append([]Event(nil), j.Events...), jsonCopy(*e))
			proposed.End = max(j.End, end)
			shots := cameraShots(proposed, c)
			if !sameTarget(last, *e) && shots[len(shots)-1].Switch <= now+50 {
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
			e.Reason = "连续击杀合并录制，按时间直接切换目标"
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
