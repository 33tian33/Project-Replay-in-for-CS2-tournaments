package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

var errMapNotReady = errors.New("等待 Linux B 路 GSI 识别相同地图")

// Remaining is the game countdown, not a system timestamp or demo tick.
// Phase distinguishes the live-round, planted-bomb and round-over clocks.
type RoundClock struct {
	Round     int     `json:"round"`
	Phase     string  `json:"phase"`
	Remaining float64 `json:"remaining"`
}
type clockSample struct {
	Clock      RoundClock `json:"clock"`
	At         int64      `json:"at"`
	ProgressAt int64      `json:"progress_at"`
}

func phaseOrder(phase string) int {
	switch phase {
	case "freezetime":
		return 0
	case "live":
		return 1
	case "bomb":
		return 2
	case "over":
		return 3
	}
	return -1
}
func validRoundClock(c *RoundClock) bool {
	return c != nil && c.Round > 0 && c.Round <= 1000 && phaseOrder(c.Phase) >= 1 && !math.IsNaN(c.Remaining) && !math.IsInf(c.Remaining, 0) && c.Remaining >= 0 && c.Remaining <= 600
}

func roundOver(p map[string]any) bool {
	return stringField(obj(p, "round"), "phase") == "over" || stringField(obj(p, "phase_countdowns"), "phase") == "over"
}

// map.round may increment at the win announcement, before players stop fighting.
// Keep the observed round through the entire over phase; advance at the next start.
func roundNumber(p, previous map[string]any, previousRound int) int {
	raw, ok := gsiCount(p, "map", "round")
	if !ok {
		return 0
	}
	if roundOver(p) {
		old, known := gsiCount(previous, "map", "round")
		if known && stringField(obj(p, "map"), "name") == stringField(obj(previous, "map"), "name") && previousRound > 0 && raw >= old && raw <= old+1 {
			return previousRound
		}
		return max(1, raw)
	}
	return raw + 1
}
func readRoundClock(p map[string]any) *RoundClock {
	phase := obj(p, "phase_countdowns")
	raw, ok := phase["phase_ends_in"]
	if !ok {
		return nil
	}
	var seconds float64
	switch v := raw.(type) {
	case string:
		var err error
		seconds, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return nil
		}
	case float64:
		seconds = v
	default:
		return nil
	}
	if _, ok := gsiCount(p, "map", "round"); !ok {
		return nil
	}
	c := &RoundClock{Round: roundNumber(p, nil, 0), Phase: stringField(phase, "phase"), Remaining: seconds}
	if c.Round <= 0 || c.Round > 1000 || phaseOrder(c.Phase) < 0 || math.IsNaN(c.Remaining) || math.IsInf(c.Remaining, 0) || c.Remaining < 0 || c.Remaining > 600 {
		return nil
	}
	return c
}
func (a *Service) updateRoundClockLocked(side string, p map[string]any, t int64) {
	a.updateRoundClockForRoundLocked(side, p, t, roundNumber(p, a.previous[side], a.gsiRound[side]+1))
}

func (a *Service) updateRoundClockForRoundLocked(side string, p map[string]any, t int64, round int) {
	if a.roundClocks == nil {
		a.roundClocks = map[string]clockSample{}
	}
	c := readRoundClock(p)
	if c == nil {
		delete(a.roundClocks, side)
		return
	}
	old := a.roundClocks[side]
	c.Round = round
	if a.phaseAnchors == nil {
		a.phaseAnchors = map[string]clockSample{}
	}
	if old.Clock.Round != c.Round {
		delete(a.phaseAnchors, side)
	} else if old.Clock.Phase != c.Phase {
		delete(a.phaseAnchors, side)
		if validRoundClock(&old.Clock) && phaseOrder(old.Clock.Phase) < phaseOrder(c.Phase) && t-old.At <= 1500 {
			a.phaseAnchors[side] = old
		}
	}
	if a.liveDuration == nil {
		a.liveDuration = map[string]float64{}
	}
	if old.Clock.Round != c.Round {
		delete(a.liveDuration, side)
	}
	if old.Clock.Round == c.Round && old.Clock.Phase == "freezetime" && c.Phase == "live" && t-old.At <= 1500 {
		elapsed := float64(t-old.At)/1000 - old.Clock.Remaining
		if elapsed >= -.2 && elapsed <= 1.5 {
			a.liveDuration[side] = c.Remaining + max(0, elapsed)
		}
	}
	progress := old.ProgressAt
	if old.Clock.Round != c.Round || old.Clock.Phase != c.Phase || old.At == 0 || c.Remaining > old.Clock.Remaining+.2 {
		progress = 0
	}
	if old.At > 0 && old.Clock.Round == c.Round && old.Clock.Phase == c.Phase && old.Clock.Remaining > c.Remaining && t-old.At < 2000 {
		progress = t
	}
	if old.Clock.Round == c.Round && phaseOrder(old.Clock.Phase) >= 1 && phaseOrder(c.Phase) > phaseOrder(old.Clock.Phase) && old.ProgressAt > 0 && t-old.ProgressAt < 1500 {
		progress = t
	}
	// A normal freeze/live boundary is continuous playback, not a pause.
	if old.Clock.Round == c.Round && old.Clock.Phase == "freezetime" && c.Phase == "live" && old.ProgressAt > 0 && t-old.ProgressAt < 1500 {
		elapsed := float64(t-old.At)/1000 - old.Clock.Remaining
		if elapsed >= -.2 && elapsed <= 1.5 {
			progress = t
		}
	}
	a.roundClocks[side] = clockSample{*c, t, progress}
}
func (a *Service) resolveRoundEventsLocked(now int64) {
	sample := a.roundClocks["b"]
	for i := range a.s.Events {
		e := &a.s.Events[i]
		if !e.RoundTiming || e.JobID != "" {
			continue
		}
		e.Time = 0
		if !validRoundClock(e.Clock) {
			e.Status = "WAITING_CLOCK"
			e.Reason = "A 路缺少有效阶段倒计时"
			continue
		}
		if sample.At == 0 || now-sample.At > 1500 || sample.ProgressAt == 0 || now-sample.ProgressAt > 1500 {
			e.Status = "WAITING_CLOCK"
			e.Reason = "等待 B 路倒计时推进"
			continue
		}
		if sample.Clock.Round > e.Clock.Round || sample.Clock.Round == e.Clock.Round && phaseOrder(sample.Clock.Phase) > phaseOrder(e.Clock.Phase) {
			e.Status = "MISSED_WINDOW"
			e.Reason = "B 路已越过目标回合或阶段"
			continue
		}
		if predicted, ok := clockPrediction(sample, *e); ok {
			e.Time = predicted
			continue
		}
		if sample.Clock.Round != e.Clock.Round || sample.Clock.Phase != e.Clock.Phase {
			e.Status = "WAITING_CLOCK"
			e.Reason = "任务已接收，等待 B 路目标回合 / 阶段"
			continue
		}
		e.Time = sample.At + int64((sample.Clock.Remaining-e.Clock.Remaining)*1000)
	}
}

// Only a name obtained from the receiver's own fresh roster may be used.
func (a *Service) playerCommandLocked(j Job, c Config) (string, error) {
	if slot := c.Mappings[j.Player]; slot > 0 {
		return fmt.Sprintf("spec_player %d", slot), nil
	}
	p := obj(a.previous["b"], "allplayers")
	name := stringField(obj(p, j.Player), "name")
	if name == "" || strings.ContainsAny(name, "\";\\\r\n") {
		return "", errors.New("B 路缺少安全可用的玩家名称，请提供已确认槽位")
	}
	if _, err := strconv.ParseFloat(name, 64); err == nil {
		return "", errors.New("纯数字玩家名需使用已确认槽位")
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return "", errors.New("玩家名含控制字符")
		}
	}
	for steam, raw := range p {
		v, _ := raw.(map[string]any)
		if steam != j.Player && strings.EqualFold(stringField(v, "name"), name) {
			return "", errors.New("B 路存在同名玩家，请提供已确认槽位")
		}
	}
	return "spec_player \"" + name + "\"", nil
}

func (a *Service) acceptRoundEvent(e Event) (Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.s.Config
	if a.role != "agent" || c.Mode != "live" {
		return e, errors.New("目标必须是 live 模式的 Linux Agent")
	}
	if e.Match != c.Match {
		return e, errors.New("两端比赛名称不一致")
	}
	if e.Map == "" || e.Map != c.Map {
		return e, errMapNotReady
	}
	if math.IsNaN(e.LiveDuration) || math.IsInf(e.LiveDuration, 0) || e.LiveDuration < 0 || e.LiveDuration > 600 || !e.RoundTiming || !validRoundClock(e.Clock) || e.Round != e.Clock.Round || e.Utility != nil || !validClockAnchor(e) {
		return e, errors.New("需要回合号和有效阶段倒计时；道具轨迹仍使用独立路径")
	}
	e.Epoch = c.Epoch
	e.Quality = "inferred"
	e.Time = 0
	e.Tick = nil
	e.TickDomain = ""
	e.JobID = ""
	e.Status = ""
	e.Reason = ""
	for _, old := range a.s.Events {
		if old.ID == e.ID {
			x, y := jsonCopy(old), jsonCopy(e)
			x.Time = 0
			x.JobID = ""
			x.Status = ""
			x.Reason = ""
			xb, _ := json.Marshal(x)
			yb, _ := json.Marshal(y)
			if string(xb) != string(yb) {
				return e, errors.New("同一事件 ID 的内容发生变化")
			}
			return old, nil
		}
	}
	if err := a.upsertLocked(e); err != nil {
		return e, err
	}
	if a.storageErr != "" {
		return e, errors.New(a.storageErr)
	}
	a.logLocked("info", fmt.Sprintf("收到自动击杀：第 %d 回合 %s %.1fs · %s", e.Round, e.Clock.Phase, e.Clock.Remaining, e.Name))
	return e, nil
}
func (a *Service) roundRoutes(api *http.ServeMux) {
	api.HandleFunc("POST /api/round-events", func(w http.ResponseWriter, r *http.Request) {
		var e Event
		if err := decode(w, r, &e); err != nil {
			fail(w, err)
			return
		}
		accepted, err := a.acceptRoundEvent(e)
		if errors.Is(err, errMapNotReady) {
			respond(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, 200, accepted)
	})
	api.HandleFunc("GET /api/round-events/{id}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, e := range a.s.Events {
			if e.ID == r.PathValue("id") {
				result := Job{Status: e.Status, Error: e.Reason}
				if e.JobID == "" && e.Time > 0 && e.Time-1000-int64(a.s.Config.Guard*1000) < nowMS() {
					result.Status = "MISSED_WINDOW"
					result.Error = "未能在目标窗口前开始录制：" + e.Reason
				}
				if e.Epoch != a.s.Config.Epoch {
					result.Status = "CANCELLED"
					result.Error = "节点会话已变化"
				} else {
					for _, j := range a.s.Jobs {
						if j.ID == e.JobID {
							result = j
							break
						}
					}
				}
				respond(w, 200, result)
				return
			}
		}
		http.NotFound(w, r)
	})
}

// Caller holds mu. Forward observations before local calibration, POV mapping or planning.
func (a *Service) forwardRoundEventsLocked(c Config) {
	if a.role != "director" || !remoteConfigured(c) || c.Mode != "live" || !c.AutoCapture || c.Paused {
		return
	}
	for i := range a.s.Events {
		e := &a.s.Events[i]
		if !e.RoundTiming || e.JobID != "" || e.Epoch != c.Epoch || e.Map != c.Map || e.Match != c.Match {
			continue
		}
		j := Job{ID: id(), Epoch: c.Epoch, Match: c.Match, Map: c.Map, Player: e.Player, Events: []Event{jsonCopy(*e)}, Status: "SENDING"}
		e.JobID = j.ID
		e.Status = "SENDING"
		e.Reason = "自动发送回合与局内时间"
		a.s.Jobs = append(a.s.Jobs, j)
		a.active = true
		a.saveLocked()
		if a.storageErr != "" {
			return
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.runRoundRemote(j, c)
			a.mu.Lock()
			a.active = a.pendingJobsLocked()
			a.mu.Unlock()
		}()
	}
}
func (a *Service) submitRoundEvent(c Config, e Event, accepted *Event) error {
	deadline := nowMS() + 120000
	for {
		a.mu.Lock()
		current := a.s.Config.Epoch == c.Epoch && a.s.Config.Map == e.Map
		a.mu.Unlock()
		if !current {
			return errors.New("地图或会话已变化，停止发送旧事件")
		}
		err := remoteCall(a.ctx, c, "POST", "/api/round-events", e, accepted)
		var remoteErr *remoteHTTPError
		if !errors.As(err, &remoteErr) || remoteErr.Status != http.StatusConflict || nowMS() >= deadline {
			return err
		}
		if err := a.waitUntil(nowMS() + 500); err != nil {
			return err
		}
	}
}

func (a *Service) runRoundRemote(j Job, c Config) {
	e := j.Events[0]
	var accepted Event
	if err := a.submitRoundEvent(c, e, &accepted); err != nil {
		var state Job
		if query := remoteCall(a.ctx, c, "GET", "/api/round-events/"+e.ID, nil, &state); query != nil {
			a.jobUpdate(j.ID, "FAILED", "击杀发送失败："+err.Error(), nil)
			return
		}
	}
	a.jobUpdate(j.ID, "QUEUED", "Linux 已接收，等待目标回合与局内时间", nil)
	last := ""
	for until := nowMS() + 600000; nowMS() < until; {
		if a.waitUntil(nowMS()+250) != nil {
			return
		}
		var state Job
		if err := remoteCall(a.ctx, c, "GET", "/api/round-events/"+e.ID, nil, &state); err != nil {
			continue
		}
		if state.Status == "READY" {
			a.jobUpdate(j.ID, "TRANSFERRING", "下载节点素材并校验", nil)
			var arts []Artifact
			for _, art := range state.Artifacts {
				if art.EventID != e.ID {
					continue
				}
				if err := a.download(c, &art); err != nil {
					a.jobUpdate(j.ID, "FAILED", err.Error(), nil)
					return
				}
				art.JobID = j.ID
				arts = append(arts, art)
			}
			if len(arts) == 0 {
				a.jobUpdate(j.ID, "FAILED", "节点结果缺少该击杀素材", nil)
				return
			}
			a.jobUpdate(j.ID, "READY", "节点素材已自动回传", arts)
			return
		}
		if terminal(state.Status) {
			a.jobUpdate(j.ID, state.Status, state.Error, nil)
			return
		}
		if state.Status != "" && state.Status+state.Error != last {
			last = state.Status + state.Error
			a.jobUpdate(j.ID, state.Status, state.Error, nil)
		}
	}
	a.jobUpdate(j.ID, "FAILED", "等待节点回合时间或素材超时", nil)
}

// Reject a stale/pause-shifted countdown rather than publish a mis-timed clip.
func (a *Service) checkRoundJobLocked(j Job) error {
	if len(j.Events) == 0 || !j.Events[0].RoundTiming {
		return nil
	}
	sample := a.roundClocks["b"]
	if sample.At == 0 || nowMS()-sample.At > 1500 || sample.ProgressAt == 0 || nowMS()-sample.ProgressAt > 1500 {
		return errors.New("B 路倒计时未推进或已断流")
	}
	for _, e := range j.Events {
		if !validRoundClock(e.Clock) {
			return errors.New("任务回合时间无效")
		}
		if predicted, ok := clockPrediction(sample, e); ok {
			if math.Abs(float64(predicted-e.Time)) > 500 {
				return errors.New("阶段锚点时间发生暂停或跳转")
			}
		} else if sample.Clock.Round == e.Clock.Round && sample.Clock.Phase == e.Clock.Phase {
			predicted := sample.At + int64((sample.Clock.Remaining-e.Clock.Remaining)*1000)
			if math.Abs(float64(predicted-e.Time)) > 500 {
				return errors.New("B 路时间线发生暂停或跳转，取消过期录制")
			}
		} else if nowMS() < e.Time-250 {
			return errors.New("B 路尚未到达任务阶段或已回档")
		}
	}
	return nil
}

func validClockAnchor(e Event) bool {
	if e.ClockAnchor == nil {
		return e.ClockOffset == 0
	}
	return validRoundClock(e.ClockAnchor) && e.Clock != nil && e.ClockAnchor.Round == e.Round && phaseOrder(e.ClockAnchor.Phase) < phaseOrder(e.Clock.Phase) && !math.IsNaN(e.ClockOffset) && !math.IsInf(e.ClockOffset, 0) && e.ClockOffset >= 0 && e.ClockOffset <= 600
}

// An A-side sample immediately before a phase change also locates early bomb/over
// kills while B is still in the preceding phase. No fixed round/bomb duration.
func clockPrediction(sample clockSample, e Event) (int64, bool) {
	if predicted, ok := freezePrediction(sample, e); ok {
		return predicted, true
	}
	if e.ClockAnchor != nil && validClockAnchor(e) && sample.Clock.Round == e.Round && sample.Clock.Phase == e.ClockAnchor.Phase {
		return sample.At + int64((sample.Clock.Remaining-e.ClockAnchor.Remaining+e.ClockOffset)*1000), true
	}
	return 0, false
}

// The A feed measures this round's live duration at the freeze/live boundary.
// B can prepare an opening kill while its own freeze countdown is still running.
func freezePrediction(sample clockSample, e Event) (int64, bool) {
	if e.Clock == nil || sample.Clock.Round != e.Clock.Round || sample.Clock.Phase != "freezetime" || e.Clock.Phase != "live" || e.LiveDuration <= 0 || e.LiveDuration < e.Clock.Remaining {
		return 0, false
	}
	return sample.At + int64((sample.Clock.Remaining+e.LiveDuration-e.Clock.Remaining)*1000), true
}
