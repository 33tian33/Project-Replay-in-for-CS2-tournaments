package replay

import "sort"

const replayBeforeMS int64 = 1000
const replayAfterMS int64 = 500
const replayDuration = float64(replayBeforeMS+replayAfterMS) / 1000

type route struct {
	indices []int
	score   [5]int
	pins    int
}

func better(a, b route) bool {
	if a.pins != b.pins {
		return a.pins > b.pins
	}
	for k := range a.score {
		if a.score[k] != b.score[k] {
			return a.score[k] > b.score[k]
		}
	}
	return false
}
func weight(e Event) [5]int {
	m := 0
	if e.Missed {
		m = 1
	}
	k := e.Kills - 1
	if k < 0 {
		k = 0
	}
	if k > 4 {
		k = 4
	}
	return [5]int{0, 1, m, k, 0}
}
func bounds(e Event, c Config) (int64, int64) {
	g := int64(c.Guard * 1000)
	return e.Time - replayBeforeMS - g, e.Time + replayAfterMS + g
}
func compatible(a, b Event, c Config) bool {
	if sameTarget(a, b) {
		return true
	}
	_, end := bounds(a, c)
	start, _ := bounds(b, c)
	return end+int64((c.Transition+c.Setup)*1000) <= start
}

// Plan is deterministic, independent of arrival order. Existing commitments are reservations.
// Manual pins are mandatory: incompatible pins are surfaced and planning stops.
func Plan(events []Event, jobs []Job, c Config, now int64) []Event {
	out := append([]Event{}, events...)
	candidates := []Event{}
	for k := range out {
		e := &out[k]
		if e.JobID != "" {
			continue
		}
		if e.RoundTiming && e.Time == 0 {
			if e.Epoch != c.Epoch || e.Match != c.Match || e.Map != c.Map {
				e.Status = "CANCELLED"
				e.Reason = "旧会话"
			} else if e.Status != "MISSED_WINDOW" {
				e.Status = "WAITING_CLOCK"
				if e.Reason == "" {
					e.Reason = "等待回合与局内时间"
				}
			}
			continue
		}
		previousStatus := e.Status
		if previousStatus == "CONFLICT" && e.Time-1000-int64(c.Guard*1000) <= now && e.Epoch == c.Epoch {
			e.Reason = "视角冲突未录制；该窗口已结束"
			continue
		}
		e.Status = "DETECTED"
		e.Reason = "等待规划"
		switch {
		case e.Epoch != c.Epoch || e.Match != c.Match || e.Map != c.Map:
			e.Status = "CANCELLED"
			e.Reason = "旧会话或时间线"
		case e.GroupCount > 1 && !e.RoundTiming:
			e.Status = "UNCERTAIN"
			e.Reason = "密集击杀待逐次定位，不生成虚构事件"
		case !resolvedUtility(e.Utility):
			e.Status = "TRACKING_UNRESOLVED"
			e.Reason = "道具类型、实例或关联证据不完整"
		case c.Mode == "live" && e.Utility != nil && c.TrackingMode == "native" && !nativeTrackReady(*e, c):
			e.Status = "TRACKING_UNRESOLVED"
			e.Reason = "道具轨迹未覆盖完整录制窗口"
		case c.Mode == "live" && c.TrackingMode != "native" && e.Utility != nil && c.TrackingURL == "" && !remoteConfigured(c):
			e.Status = "TRACKING_UNAVAILABLE"
			e.Reason = "未配置道具镜头适配器，不能用投掷者视角代替"
		case e.Player == "":
			e.Status = "IDENTITY_UNKNOWN"
			e.Reason = "目标身份未知"
		case c.Mode == "live" && e.Utility == nil && c.Mappings[e.Player] == 0 && !e.RoundTiming:
			e.Status = "IDENTITY_UNKNOWN"
			e.Reason = "B 路命令槽位未确认"
		case e.Uncertainty > c.Guard:
			e.Status = "UNCERTAIN"
			e.Reason = "时间误差超过保护量"
		case c.Mode == "live" && c.Strict && !e.RoundTiming:
			e.Status = "UNCERTAIN"
			e.Reason = "严格模式等待事件到视频帧的定位适配器；本版仅支持近似采集"
		case e.Time-1000-int64(c.Guard*1000) <= now:
			e.Status = "MISSED_WINDOW"
			e.Reason = "已错过完整窗口准备期限"
		case c.Paused:
			e.Reason = "采集已暂停"
		case c.Mode == "live" && c.CalibratedUntil <= now && !e.RoundTiming:
			e.Reason = "校准失效，请重新校准"
		default:
			conflict := false
			a, b := bounds(*e, c)
			for _, j := range jobs {
				if !reservesCamera(j.Status) {
					continue
				}
				margin := int64((c.Transition + c.Setup) * 1000)
				if a < j.End+margin && b+margin > j.Start {
					conflict = true
					break
				}
			}
			if conflict {
				e.Status = "CONFLICT"
				e.Reason = "与已提交任务冲突，保留已承诺片段"
			} else {
				candidates = append(candidates, *e)
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Time != b.Time {
			return a.Time < b.Time
		}
		if a.Player != b.Player {
			return a.Player < b.Player
		}
		return a.ID < b.ID
	})
	pins := []Event{}
	for _, e := range candidates {
		if e.Pin {
			pins = append(pins, e)
		}
	}
	for i := 1; i < len(pins); i++ {
		if !compatible(pins[i-1], pins[i], c) {
			for k := range out {
				if out[k].JobID == "" && out[k].Pin {
					out[k].Status = "PIN_CONFLICT"
					out[k].Reason = "手动指定窗口冲突，请取消其中一个"
				}
			}
			return out
		}
	}
	dp := make([]route, len(candidates))
	best := route{}
	for j, e := range candidates {
		w := priorityWeight(e, candidates)
		p := 0
		if e.Pin {
			p = 1
		}
		dp[j] = route{indices: []int{j}, score: w, pins: p}
		for i := 0; i < j; i++ {
			if !compatible(candidates[i], e, c) {
				continue
			}
			r := route{indices: append(append([]int(nil), dp[i].indices...), j), score: dp[i].score, pins: dp[i].pins + p}
			for k := range w {
				r.score[k] += w[k]
			}
			if !sameTarget(candidates[i], e) {
				r.score[4]--
			}
			if better(r, dp[j]) {
				dp[j] = r
			}
		}
		if better(dp[j], best) {
			best = dp[j]
		}
	}
	selected := map[string]bool{}
	for _, i := range best.indices {
		selected[candidates[i].ID] = true
	}
	eligible := map[string]bool{}
	for _, e := range candidates {
		eligible[e.ID] = true
	}
	for k := range out {
		e := &out[k]
		if selected[e.ID] {
			e.Status = "SCHEDULED"
			e.Reason = "完整覆盖优先 · 暂定计划"
		} else if eligible[e.ID] {
			e.Status = "CONFLICT"
			e.Reason = "视角窗口冲突，优先覆盖更多完整击杀"
		}
	}
	return out
}

func absEventGap(a, b Event) int64 {
	gap := a.Time - b.Time
	if gap < 0 {
		return -gap
	}
	return gap
}

func reservesCamera(status string) bool {
	return !terminal(status) && status != "FINALIZING" && status != "TRANSFERRING"
}

func priorityWeight(e Event, candidates []Event) [5]int {
	w := weight(e)
	if e.RoundTiming && e.GroupCount > 1 {
		w[0] = 1
	}
	if e.Clutch == 2 || e.Clutch == 3 {
		w[0] = 2
	}
	for _, other := range candidates {
		if other.ID != e.ID && other.Round == e.Round && sameTarget(other, e) && absEventGap(other, e) <= 2000 {
			w[0] = max(w[0], 1)
		}
	}
	return w
}
