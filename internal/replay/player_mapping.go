package replay

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Automatic slots belong to one live B roster, never to saved configuration.
type playerMapping struct {
	Phase    string         `json:"phase"`
	Message  string         `json:"message"`
	Slots    map[string]int `json:"slots"`
	Total    int            `json:"total"`
	Slot     int            `json:"slot"`
	key      string
	retryAt  int64
	deadline int64
}

func (a *Service) mappingKeyLocked() (string, []string) {
	ids := []string{}
	for id := range obj(a.previous["b"], "allplayers") {
		n, err := strconv.ParseUint(id, 10, 64)
		if err == nil && n >= 76561197960265728 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return fmt.Sprintf("%d/%s/%s/%s/%s", a.s.Config.Epoch, a.s.Config.Map, a.s.Config.NetCon, stringField(obj(a.previous["b"], "provider"), "steamid"), strings.Join(ids, ",")), ids
}

func (a *Service) refreshPlayerMappingLocked(reset bool) {
	key, ids := a.mappingKeyLocked()
	if reset || a.playerMapping == nil || a.playerMapping.key != key {
		a.playerMapping = &playerMapping{key: key, Total: len(ids), Slots: map[string]int{}, Phase: "waiting", Message: "等待 B 路空闲后自动确认玩家槽位"}
	}
}

func (a *Service) captureConfigLocked() Config {
	c := a.s.Config
	if a.role != "agent" {
		return c
	}
	c.requirePlayerMappings = true
	c.Mappings = jsonCopy(c.Mappings)
	if c.Mappings == nil {
		c.Mappings = map[string]int{}
	}
	a.refreshPlayerMappingLocked(false)
	used := map[int]bool{}
	for _, slot := range c.Mappings {
		used[slot] = true
	}
	for player, slot := range a.playerMapping.Slots {
		if c.Mappings[player] == 0 && !used[slot] {
			c.Mappings[player] = slot
			used[slot] = true
		}
	}
	return c
}

// Called with mu held. Camera ownership is acquired before launching, so no
// committed capture or another mapping pass can race the discovery commands.
func (a *Service) startPlayerMappingLocked(now int64) bool {
	if a.mappingBusy {
		return true
	}
	c := a.s.Config
	if a.role != "agent" || c.Mode != "live" || c.Paused || !c.AutoCapture || a.ctx.Err() != nil {
		return false
	}
	a.refreshPlayerMappingLocked(false)
	run := a.playerMapping
	if run.Total < 2 || now-a.gsiSeen["b"] >= 1500 || a.observed == "" || now < run.retryAt {
		return false
	}
	effective := a.captureConfigLocked()
	_, ids := a.mappingKeyLocked()
	missing := false
	for _, id := range ids {
		if effective.Mappings[id] == 0 {
			missing = true
		}
	}
	if !missing {
		run.Phase = "ready"
		run.Message = fmt.Sprintf("命令槽位已就绪：%d 名玩家（手动映射优先）", run.Total)
		return false
	}
	if a.mappingWouldDelayCaptureLocked(now) {
		return false
	}
	if a.active || a.localDemo.Phase == "armed" || a.localDemo.Phase == "running" || a.cs2Launch.Phase == "starting" {
		return false
	}
	for _, j := range a.s.Jobs {
		if reservesCamera(j.Status) {
			return false
		}
	}
	if !a.captureMu.TryLock() {
		return false
	}
	a.mappingBusy = true
	run.deadline = now + 6000
	run.Phase = "scanning"
	run.Message = "正在自动确认 B 路命令槽位，完成后恢复录制"
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer a.captureMu.Unlock()
		err := a.discoverPlayerSlots(run, c, ids)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.mappingBusy = false
		if a.playerMapping != run {
			return
		}
		run.retryAt = nowMS() + 30000
		run.Phase = "ready"
		run.Message = fmt.Sprintf("自动槽位已确认 %d/%d 名玩家", len(run.Slots), run.Total)
		if err != nil {
			run.Phase = "waiting"
			run.Message = "自动映射等待重试：" + err.Error()
		}
		a.logLocked("info", run.Message)
	}()
	return true
}

func (a *Service) mappingObservation(run *playerMapping) (string, int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil {
		return "", 0, a.ctx.Err()
	}
	if a.playerMapping != run || a.s.Config.Paused {
		return "", 0, errors.New("会话或名单变化，取消本次探测")
	}
	if nowMS() >= run.deadline || a.mappingWouldDelayCaptureLocked(nowMS()) {
		return "", 0, errors.New("探测时间片结束或录制即将开始，保留已确认槽位并释放相机")
	}
	if nowMS()-a.gsiSeen["b"] >= 1500 || a.observed == "" {
		return "", 0, errors.New("B 路观战数据不新鲜")
	}
	return a.observed, a.observedAt, nil
}

// Mapping shares the camera with recording. Yield before a known target's
// preparation deadline, including events that arrived during discovery.
func (a *Service) mappingWouldDelayCaptureLocked(now int64) bool {
	c := a.captureConfigLocked()
	sample := a.roundClocks["b"]
	if sample.At == 0 || now-sample.At > 1500 {
		return false
	}
	for _, e := range a.s.Events {
		if e.JobID != "" || e.Epoch != c.Epoch || e.Map != c.Map || c.Mappings[e.Player] == 0 || !validRoundClock(e.Clock) {
			continue
		}
		at, ok := clockPrediction(sample, e)
		if !ok && sample.Clock.Round == e.Round && sample.Clock.Phase == e.Clock.Phase {
			at, ok = sample.At+int64((sample.Clock.Remaining-e.Clock.Remaining)*1000), true
		}
		if ok && at > now && at-now <= recordingSetupMS(c)+replayBeforeMS+int64(c.Guard*1000)+1500 {
			return true
		}
	}
	return false
}

func (a *Service) discoverPlayerSlots(run *playerMapping, c Config, ids []string) error {
	release, err := resourceLock(c.SessionLock)
	if err != nil {
		return err
	}
	defer release()
	n, err := openConsole(c.NetCon)
	if err != nil {
		return err
	}
	defer n.conn.Close()
	if _, err = n.queryWithTimeout("spec_autodirector 0; spec_mode 1", time.Second); err != nil {
		return err
	}
	probe := func(slot int) (string, error) {
		before, _, err := a.mappingObservation(run)
		if err != nil {
			return "", err
		}
		if _, err = n.queryWithTimeout(fmt.Sprintf("spec_player %d", slot), time.Second); err != nil {
			return "", err
		}
		// Require two distinct GSI frames after console acknowledgement, with a
		// changed identity. Invalid slots usually leave the current target untouched.
		acknowledged := nowMS()
		lastID := ""
		lastAt := int64(0)
		for nowMS()-acknowledged < 1200 {
			if err := a.waitUntil(nowMS() + 40); err != nil {
				return "", err
			}
			player, at, err := a.mappingObservation(run)
			if err != nil {
				return "", err
			}
			if at <= acknowledged {
				lastID = ""
				lastAt = 0
				continue
			}
			if player == lastID && at > lastAt && at-acknowledged >= 200 {
				if player == before {
					return "", nil
				}
				return player, nil
			}
			if player != lastID {
				lastID = player
				lastAt = at
			}
		}
		return "", nil
	}
	a.mu.Lock()
	start, seed := run.Slot, jsonCopy(run.Slots)
	a.mu.Unlock()
	verified, err := discoverSlotsFrom(ids, probe, func(slot int) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.playerMapping == run {
			run.Slot = slot%128 + 1
		}
	}, start, seed)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.playerMapping != run {
		return errors.New("会话变化，丢弃旧槽位")
	}
	for player, slot := range verified {
		run.Slots[player] = slot
	}
	if err == nil && len(verified) < len(ids) {
		return errors.New("部分玩家暂时无法观战，将在空闲时重试")
	}
	return err
}

// Every accepted pair is reproduced from a different target. A no-op command
// or one late identity sample cannot establish a mapping on its own.
func discoverSlots(ids []string, probe func(int) (string, error), progress func(int)) (map[string]int, error) {
	return discoverSlotsFrom(ids, probe, progress, 1, nil)
}

func discoverSlotsFrom(ids []string, probe func(int) (string, error), progress func(int), start int, seed map[string]int) (map[string]int, error) {
	roster := map[string]bool{}
	for _, id := range ids {
		roster[id] = true
	}
	candidates := map[string]int{}
	verified := map[string]int{}
	for player, slot := range seed {
		if roster[player] {
			candidates[player], verified[player] = slot, slot
		}
	}
	if start < 1 || start > 128 {
		start = 1
	}
	// A second pass discovers a slot that was already observed before pass one.
	for pass := 0; pass < 2; pass++ {
		for offset := 0; offset < 128; offset++ {
			slot := (start-1+offset)%128 + 1
			progress(slot)
			player, err := probe(slot)
			if err != nil {
				return verified, err
			}
			if !roster[player] {
				continue
			}
			candidates[player] = slot
			for _, other := range ids {
				otherSlot := candidates[other]
				if other == player || otherSlot == 0 {
					continue
				}
				switched, err := probe(otherSlot)
				if err != nil {
					return verified, err
				}
				if switched != other {
					continue
				}
				returned, err := probe(slot)
				if err != nil {
					return verified, err
				}
				if returned != player {
					continue
				}
				// Both directions were observed, including a repeat of each selection.
				again, err := probe(otherSlot)
				if err != nil {
					return verified, err
				}
				if again == other {
					verified[player] = slot
					verified[other] = otherSlot
				}
				break
			}
			if len(verified) == len(roster) {
				return verified, nil
			}
		}
	}
	return verified, nil
}
