package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"projectreplay/internal/relay"
	"sort"
	"sync"
	"time"
)

type Service struct {
	phaseAnchors      map[string]clockSample
	relayState        relayStatus
	hudHidden         bool
	maintenance       sync.RWMutex
	liveDuration      map[string]float64
	manualRounds      map[int]bool
	roundClocks       map[string]clockSample
	gsiDiagnostics    map[string]GSIDiagnostic
	hudImport         CS2Launch
	obsLaunch         CS2Launch
	cs2Launch         CS2Launch
	localDemo         LocalDemo
	output            OutputSession
	outputSeen        int64
	outputClient      string
	outputError       string
	hotkeyStatus      string
	endRound          int
	fullTriggered     bool
	halfTriggered     bool
	closeOnce         sync.Once
	releaseData       func()
	mu                sync.Mutex
	s                 State
	dir               string
	role              string
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	active            bool
	captureMu         sync.Mutex
	remoteEpoch       int
	storageErr        string
	observed          string
	observedAt        int64
	previous          map[string]map[string]any
	gsiMap            map[string]string
	gsiSeen           map[string]int64
	gsiRound          map[string]int
	playback          string
	liveScene         string
	playbackMu        sync.Mutex
	remoteOffset      int64
	remoteUncertainty int64
	remoteAt          int64
}

func New(dir, role string) (*Service, error) {
	if role != "director" && role != "agent" {
		return nil, errors.New("role 必须是 director 或 agent")
	}
	abs, e := filepath.Abs(dir)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(filepath.Join(abs, "media"), 0700); e != nil {
		return nil, e
	}
	releaseData, e := dataLock(filepath.Join(abs, "instance.lock"))
	if e != nil {
		return nil, e
	}
	initialized := false
	defer func() {
		if !initialized {
			releaseData()
		}
	}()
	// Retire the credential file created by older releases.
	if err := os.Remove(filepath.Join(abs, "access-token")); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Service{dir: abs, role: role, ctx: ctx, cancel: cancel, previous: map[string]map[string]any{}, gsiMap: map[string]string{}, gsiSeen: map[string]int64{}, gsiRound: map[string]int{}}
	a.s = State{Output: defaultOutput(), Config: defaults(), Events: []Event{}, Jobs: []Job{}, Artifacts: []Artifact{}, Queue: []string{}, Logs: []Log{}}
	raw, e := os.ReadFile(filepath.Join(abs, "state.json"))
	if e == nil {
		d := diskState{State: State{Output: defaultOutput(), Config: defaults()}}
		if e = json.Unmarshal(raw, &d); e != nil {
			return nil, fmt.Errorf("状态文件损坏，保留原文件: %w", e)
		}
		var legacy struct {
			State struct {
				Config struct {
					AutoCapture     *bool `json:"auto_capture"`
					WorkflowVersion *int  `json:"workflow_version"`
				} `json:"config"`
			} `json:"state"`
		}
		if err := json.Unmarshal(raw, &legacy); err == nil && legacy.State.Config.AutoCapture == nil {
			d.State.Config.Strict = false
		}
		if legacy.State.Config.WorkflowVersion == nil && d.State.Config.AutoCapture && d.State.Config.WorkerURL != "" && d.State.Config.Mode == "demo" {
			d.State.Config.Mode = "live"
		}
		d.State.Config.WorkflowVersion = 1
		a.s = d.State
		for i := range a.s.Artifacts {
			a.s.Artifacts[i].Path = d.Paths[a.s.Artifacts[i].ID]
		}
		a.s.Config.Paused = !a.s.Config.AutoCapture
		a.s.Config.CalibratedUntil = 0
		for i := range a.s.Jobs {
			j := &a.s.Jobs[i]
			if !terminal(j.Status) {
				j.Status = "FAILED"
				j.Error = "进程已重启：不重录过期实时窗口，请核对 OBS 录制状态"
				for k := range a.s.Events {
					if a.s.Events[k].JobID == j.ID {
						a.s.Events[k].Status = "FAILED"
						a.s.Events[k].Reason = j.Error
					}
				}
			}
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if a.s.Config.RelayDevice == "" {
		a.s.Config.RelayDevice = relay.ID()
	}
	if a.s.HighlightsVersion == 0 {
		for _, v := range a.s.Artifacts {
			if a.currentArtifactLocked(v) {
				if v.Round <= a.s.Output.HalfRound {
					a.s.HalfQueue = appendUnique(a.s.HalfQueue, v.ID)
				}
				a.s.FullQueue = appendUnique(a.s.FullQueue, v.ID)
			}
		}
		a.s.HighlightsVersion = 1
	}
	a.s.HUD = a.s.HUD.normalized()
	if a.s.HUDActiveSource != "" {
		a.s.TeamHUD = true
	}
	// Migrate the former two-second warmup / transition defaults.
	if a.s.Config.Setup == 2 && a.s.Config.Transition == 2 {
		a.s.Config.Setup = .2
		a.s.Config.Transition = 0
	}
	if a.s.Config.Mappings == nil {
		a.s.Config.Mappings = map[string]int{}
	}
	if e = normalizeGOTV(&a.s.Config, Config{}); e != nil {
		return nil, e
	}
	if e = a.s.Config.validate(); e != nil {
		return nil, e
	}
	a.logLocked("info", "Project Replay 已启动；回合时钟任务按自动采集设置执行")
	a.saveLocked()
	if a.storageErr != "" {
		return nil, errors.New(a.storageErr)
	}
	a.releaseData = releaseData
	initialized = true
	return a, nil
}
func (a *Service) Start() {
	a.wg.Add(1)
	go a.relayLoop()
	a.startHotkeys()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-t.C:
				a.tick()
			}
		}
	}()
	if a.role == "director" {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			timer := time.NewTicker(20 * time.Second)
			defer timer.Stop()
			for {
				select {
				case <-a.ctx.Done():
					return
				case <-timer.C:
					a.mu.Lock()
					configured := remoteConfigured(a.s.Config)
					a.mu.Unlock()
					if configured {
						if err := a.syncRemote(); err != nil {
							a.mu.Lock()
							a.logLocked("warn", "节点同步失败："+err.Error())
							a.mu.Unlock()
						}
					}
				}
			}
		}()
	}

}
func (a *Service) Close() {
	a.closeOnce.Do(func() {
		a.cancel()
		a.wg.Wait()
		a.mu.Lock()
		a.s.Config.Paused = true
		a.saveLocked()
		a.mu.Unlock()
		if a.releaseData != nil {
			a.releaseData()
		}
	})
}

func (a *Service) logLocked(level, message string) {
	a.s.Logs = append(a.s.Logs, Log{nowMS(), level, message})
	if len(a.s.Logs) > 200 {
		a.s.Logs = a.s.Logs[len(a.s.Logs)-200:]
	}
}
func (a *Service) saveLocked() {
	paths := map[string]string{}
	for _, v := range a.s.Artifacts {
		paths[v.ID] = v.Path
	}
	if e := saveJSON(filepath.Join(a.dir, "state.json"), diskState{a.s, paths}); e != nil {
		a.storageErr = e.Error()
		a.s.Config.Paused = true
	} else {
		a.storageErr = ""
	}
}
func (a *Service) snapshot() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := jsonCopy(a.s)
	s.Config.OBSPassword = ""
	s.Config.GOTVPassword = ""
	side := "a"
	if a.role == "agent" {
		side = "b"
	}
	return map[string]any{"relay": a.relayState, "gsi": jsonCopy(a.gsiDiagnostics), "hud_import": a.hudImport, "obs_launch": a.obsLaunch, "cs2_launch": a.cs2Launch, "local_demo": a.localDemo, "output": a.output, "output_online": nowMS()-a.outputSeen < 3000, "output_error": a.outputError, "hotkey_status": a.hotkeyStatus, "current_round": a.gsiRound[side] + 1, "output_url": "/output.html", "version": Version, "role": a.role, "state": s, "now": nowMS(), "observed": a.observed, "observed_at": a.observedAt, "round_clocks": jsonCopy(a.roundClocks), "storage_error": a.storageErr, "playback": a.playback, "remote_at": a.remoteAt, "remote_uncertainty_ms": a.remoteUncertainty}
}
func (a *Service) configure(c Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if (a.obsLaunch.Phase == "starting" || a.hudImport.Phase == "starting") || a.cs2Launch.Phase == "starting" || a.active || a.localDemo.Phase == "armed" || a.localDemo.Phase == "running" {
		return errors.New("请先停止 Demo 联调；已有提交或录制任务，请等待完成再修改配置")
	}
	old := a.s.Config
	c.RelayURL = relay.NormalizeURL(c.RelayURL)
	c.RelayDevice = old.RelayDevice
	c.RelayPair = old.RelayPair
	if c.ConnectionMode != old.ConnectionMode || c.RelayURL != old.RelayURL || c.RelayDevice != old.RelayDevice || c.RelayGroup != old.RelayGroup {
		c.RelayPair = ""
	}
	if err := normalizeGOTV(&c, old); err != nil {
		return err
	}
	// Map is observed state, never an editable session setting.
	c.Map = old.Map
	if c.OBSPassword == "" {
		c.OBSPassword = old.OBSPassword
	}
	if e := c.validate(); e != nil {
		return e
	}
	c.WorkflowVersion = 1
	c.Epoch = max(old.Epoch+1, c.Epoch)
	c.Paused = !c.AutoCapture
	c.CalibratedUntil = 0
	if relayIdentity(old) != relayIdentity(c) {
		a.relayState = relayStatus{}
	}
	a.s.Config = c
	a.localDemo = LocalDemo{}
	a.roundClocks = nil
	a.phaseAnchors = nil
	a.liveDuration = nil
	a.s.Queue = nil
	a.s.HalfQueue = nil
	a.s.FullQueue = nil
	a.output = OutputSession{}
	a.endRound = 0
	a.manualRounds = nil
	a.halfTriggered = false
	a.fullTriggered = false
	a.previous = map[string]map[string]any{}
	a.gsiMap = map[string]string{}
	a.gsiSeen = map[string]int64{}
	a.gsiRound = map[string]int{}
	a.observed = ""
	a.observedAt = 0
	a.remoteAt = 0
	a.s.Events = Plan(a.s.Events, a.s.Jobs, c, nowMS())
	a.logLocked("info", "配置已保存；自动击杀使用回合与局内时间，无需 tick 校准")
	a.saveLocked()
	if a.storageErr != "" {
		return errors.New(a.storageErr)
	}
	return nil
}
func (a *Service) upsert(e Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.upsertLocked(e)
}
func (a *Service) upsertLocked(e Event) error {
	c := a.s.Config
	if e.RoundTiming {
		if e.Utility != nil || !validClockAnchor(e) || e.Clock != nil && (!validRoundClock(e.Clock) || e.Round != e.Clock.Round) {
			return errors.New("回合时间或事件类型无效")
		}
		e.Time = 0
		e.Tick = nil
		e.TickDomain = ""
		e.Quality = "inferred"
	}
	if e.ID == "" {
		e.ID = id()
	}
	if !validID(e.ID) {
		return errors.New("事件 ID 无效")
	}
	if e.Match == "" {
		e.Match = c.Match
	}
	if e.Map == "" {
		e.Map = c.Map
	}
	if e.Epoch == 0 {
		e.Epoch = c.Epoch
	}
	if e.Epoch != c.Epoch || e.Match != c.Match || e.Map != c.Map {
		return errors.New("事件不属于当前会话")
	}
	if e.Player == "" || len(e.Player) > 100 || len(e.Name) > 200 || !e.RoundTiming && (e.Time < nowMS()-60000 || e.Time > nowMS()+300000) || e.Uncertainty < 0 || e.Uncertainty > 10 {
		return errors.New("事件身份、时间或误差无效")
	}
	if err := validateUtility(e.Utility); err != nil {
		return err
	}
	if e.Quality == "" {
		e.Quality = "inferred"
	}
	if e.Quality != "inferred" && e.Quality != "verified" && e.Quality != "demo" {
		return errors.New("未知事件来源质量")
	}
	if e.Quality == "verified" && (e.Tick == nil || e.TickDomain == "" || e.Evidence == "") {
		return errors.New("核验事件必须提供 tick、tick_domain 和 evidence")
	}
	e.JobID = ""
	e.Status = "DETECTED"
	e.Reason = ""
	for i, v := range a.s.Events {
		if v.ID == e.ID {
			if v.JobID != "" {
				return errors.New("已提交事件不可修改")
			}
			a.s.Events[i] = e
			a.saveLocked()
			return nil
		}
	}
	if len(a.s.Events) >= 5000 {
		return errors.New("事件上限 5000，请归档并使用新数据目录")
	}
	a.s.Events = append(a.s.Events, e)
	a.saveLocked()
	return nil
}
func (a *Service) tick() {
	a.maintenance.RLock()
	defer a.maintenance.RUnlock()
	a.mu.Lock()
	a.tickDemoLocked()
	a.tickOutputLocked()
	if a.storageErr != "" || (a.obsLaunch.Phase == "starting" || a.hudImport.Phase == "starting") {
		a.mu.Unlock()
		return
	}
	c := a.s.Config
	t := nowMS()
	a.forwardRoundEventsLocked(c)
	if a.role == "agent" {
		a.resolveRoundEventsLocked(t)
		a.extendCapturesLocked(c, t)
	}
	a.s.Events = Plan(a.s.Events, a.s.Jobs, c, t)
	selected := []Event{}
	for _, e := range a.s.Events {
		if e.Status == "SCHEDULED" && e.JobID == "" {
			selected = append(selected, e)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].Time == selected[j].Time {
			return selected[i].ID < selected[j].ID
		}
		return selected[i].Time < selected[j].Time
	})
	if len(selected) == 0 {
		a.mu.Unlock()
		return
	}
	first := selected[0]
	start, end := bounds(first, c)
	// Keep a short rolling planning horizon, with time for network submission.
	lead := int64(3000)
	if first.RoundTiming {
		lead = 350
	} // Track B countdown until just before capture.
	if start-int64(c.Setup*1000) > t+lead {
		a.mu.Unlock()
		return
	}
	events := []Event{first}
	for _, e := range selected[1:] {
		s, b := bounds(e, c)
		last := events[len(events)-1]
		if e.Round != first.Round || (first.Utility != nil || e.Utility != nil) && !sameTarget(e, first) || !compatible(last, e, c) || s > end+1500 || b-start > 30000 {
			break
		}
		if c.TrackingMode == "native" && first.Utility != nil {
			if _, err := mergedUtility(append(append([]Event(nil), events...), e)); err != nil {
				break
			}
		}
		events = append(events, e)
		end = max(end, b)
	}
	j := Job{Utility: first.Utility, ID: id(), Epoch: c.Epoch, Match: c.Match, Map: c.Map, Player: first.Player, Slot: c.Mappings[first.Player], Events: events, Start: start, End: end, Status: "COMMITTED", Demo: c.Mode == "demo", Artifacts: []Artifact{}}
	if c.TrackingMode == "native" && first.Utility != nil {
		j.Utility, _ = mergedUtility(events)
	}
	a.s.Jobs = append(a.s.Jobs, j)
	for k := range a.s.Events {
		for _, e := range events {
			if a.s.Events[k].ID == e.ID {
				a.s.Events[k].JobID = j.ID
				a.s.Events[k].Status = "COMMITTED"
				a.s.Events[k].Reason = "已锁定，不抢占"
			}
		}
	}
	a.active = true
	a.logLocked("info", "已提交录制："+first.Name)
	a.saveLocked()
	if a.storageErr != "" {
		a.active = false
		a.mu.Unlock()
		return
	}
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		if remoteConfigured(c) && a.role == "director" {
			a.runRemote(j, c)
		} else {
			a.runJob(j, c)
		}
		a.mu.Lock()
		a.active = a.pendingJobsLocked()
		a.mu.Unlock()
	}()
}
func (a *Service) jobUpdate(jobID, status, reason string, artifacts []Artifact) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.s.Jobs {
		if a.s.Jobs[i].ID == jobID {
			a.s.Jobs[i].Status = status
			a.s.Jobs[i].Error = reason
			if artifacts != nil {
				a.s.Jobs[i].Artifacts = artifacts
			}
		}
	}
	for i := range a.s.Events {
		if a.s.Events[i].JobID == jobID {
			a.s.Events[i].Status = status
			a.s.Events[i].Reason = reason
		}
	}
	if status == "READY" {
		for _, v := range artifacts {
			exists := false
			for _, x := range a.s.Artifacts {
				if x.ID == v.ID {
					exists = true
				}
			}
			if !exists {
				a.s.Artifacts = append(a.s.Artifacts, v)
				if a.currentArtifactLocked(v) {
					a.s.Queue = append(a.s.Queue, v.ID)
					if v.Round <= a.s.Output.HalfRound {
						a.s.HalfQueue = appendUnique(a.s.HalfQueue, v.ID)
					}
					a.s.FullQueue = appendUnique(a.s.FullQueue, v.ID)
				}
			}
		}
	}
	a.logLocked("info", status+" "+reason)
	a.saveLocked()
}
func (a *Service) waitUntil(t int64) error {
	wait := time.Until(time.UnixMilli(t))
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-a.ctx.Done():
		return a.ctx.Err()
	}
}

// Caller holds mu. Finalization is still pending work, but does not own the camera.
func (a *Service) pendingJobsLocked() bool {
	for _, j := range a.s.Jobs {
		if !terminal(j.Status) {
			return true
		}
	}
	return false
}
