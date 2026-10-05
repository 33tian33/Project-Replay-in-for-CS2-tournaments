package replay

import (
	"fmt"
	"math"
	"time"
)

type GSIDiagnostic struct {
	ReceivedAt int64  `json:"received_at"`
	AcceptedAt int64  `json:"accepted_at"`
	Error      string `json:"error"`
	Players    int    `json:"players"`
	Counters   int    `json:"counters"`
	Detected   int    `json:"detected"`
}

func gsiCount(player map[string]any, group, key string) (int, bool) {
	v, ok := obj(player, group)[key].(float64)
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100000 || math.Trunc(v) != v {
		return 0, false
	}
	return int(v), true
}
func gsiKillDelta(before, after map[string]any) (delta, kills int, field string, ok bool) {
	for _, pair := range [][2]string{{"state", "round_kills"}, {"match_stats", "kills"}} {
		x, yes := gsiCount(before, pair[0], pair[1])
		y, yes2 := gsiCount(after, pair[0], pair[1])
		if yes && yes2 {
			return y - x, y, pair[0] + "." + pair[1], true
		}
	}
	return
}
func (a *Service) ingestGSI(side string, payload map[string]any) (err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.gsiDiagnostics == nil {
		a.gsiDiagnostics = map[string]GSIDiagnostic{}
	}
	d := a.gsiDiagnostics[side]
	d.ReceivedAt = nowMS()
	d.Error = ""
	d.Players = len(obj(payload, "allplayers"))
	d.Counters = 0
	for _, raw := range obj(payload, "allplayers") {
		player, _ := raw.(map[string]any)
		_, x := gsiCount(player, "state", "round_kills")
		_, y := gsiCount(player, "match_stats", "kills")
		if x || y {
			d.Counters++
		}
	}
	defer func() {
		if err != nil {
			d.Error = err.Error()
		} else {
			d.AcceptedAt = nowMS()
		}
		a.gsiDiagnostics[side] = d
	}()
	provider := obj(payload, "provider")
	if number(provider, "appid") != 730 {
		return fmt.Errorf("GSI appid 必须为 730")
	}
	t := nowMS()
	gameMap := stringField(obj(payload, "map"), "name")
	previousRound := a.gsiRound[side] + 1
	round := roundNumber(payload, a.previous[side], previousRound) - 1
	if gameMap == "" {
		// Menu/loading snapshots must not create kills against the last known map.
		delete(a.previous, side)
		delete(a.roundClocks, side)
		delete(a.phaseAnchors, side)
		return nil
	}
	authority := "a"
	if a.role == "agent" {
		authority = "b"
	}
	if gameMap != a.s.Config.Map {
		if side != authority {
			d.Error = "忽略非本机主路的异图 GSI；等待本机主路识别地图"
			return nil
		}
		a.adoptMapLocked(gameMap)
	}
	if previous := a.gsiSeen[side]; side == authority && previous > 0 && ((a.gsiMap[side] != "" && gameMap != a.gsiMap[side]) || round < a.gsiRound[side]) {
		a.s.Config.Epoch++
		a.s.Queue = nil
		a.s.HalfQueue = nil
		a.s.FullQueue = nil
		a.output = OutputSession{}
		a.endRound = 0
		a.manualRounds = nil
		a.halfTriggered = false
		a.fullTriggered = false
		a.s.Config.CalibratedUntil = 0
		a.s.Config.Paused = !a.s.Config.AutoCapture
		delete(a.roundClocks, side)
		delete(a.phaseAnchors, side)
		delete(a.previous, side)
		a.logLocked("warn", "GSI 地图变化或回档：旧时间线已失效")
	}
	if previous := a.gsiSeen[side]; previous > 0 && t-previous > 5000 {
		// Resume a fresh detection baseline without cancelling delayed B events.
		delete(a.previous, side)
		delete(a.roundClocks, side)
		delete(a.liveDuration, side)
		delete(a.phaseAnchors, side)
		a.logLocked("warn", "GSI 断流后恢复：保留待录任务，重新建立计数和时钟基线")
	}
	a.updateRoundClockForRoundLocked(side, payload, t, round+1)
	a.gsiMap[side] = gameMap
	a.gsiSeen[side] = t
	a.gsiRound[side] = round
	if side == "b" {
		if a.role == "director" {
			d.Error = "导播端收到 B 路 GSI；自动击杀需要 A 路配置 /gsi/a"
		}
		a.previous[side] = payload
		a.observed = stringField(obj(payload, "player"), "steamid")
		a.observedAt = t
		return nil
	}
	a.detectOutputLocked(payload)
	if a.s.Config.EventSource == "parser" {
		a.previous[side] = payload
		return nil
	}
	players := obj(payload, "allplayers")
	old := a.previous[side]
	a.previous[side] = payload
	if old == nil {
		return nil
	}
	oldPlayers := obj(old, "allplayers")
	oldRound := previousRound - 1
	if oldRound > round {
		return nil
	}
	for steam, raw := range players {
		player, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		previous, ok := oldPlayers[steam].(map[string]any)
		if !ok {
			continue
		}
		diff, after, field, known := gsiKillDelta(previous, player)
		if oldRound != round {
			// Round counters reset. A fresh live snapshot can already contain the opening kill.
			if n, ok := gsiCount(player, "state", "round_kills"); ok && stringField(obj(payload, "round"), "phase") != "over" && stringField(obj(payload, "phase_countdowns"), "phase") != "over" {
				diff, after, field, known = n, n, "state.round_kills", true
			} else {
				x, xok := gsiCount(previous, "match_stats", "kills")
				y, yok := gsiCount(player, "match_stats", "kills")
				diff, after, field, known = y-x, y-x, "match_stats.kills", xok && yok
			}
		}
		if !known {
			continue
		}
		if field == "match_stats.kills" {
			// Lifetime kills supply the delta only; do not use them as round weight.
			after = diff
			for _, event := range a.s.Events {
				if event.Epoch == a.s.Config.Epoch && event.Round == round+1 && event.Player == steam {
					after = max(after, event.Kills+diff)
				}
			}
		}
		if diff <= 0 {
			continue
		}
		e := Event{Clutch: clutchOpponents(oldPlayers, steam), ID: id(), Match: a.s.Config.Match, Map: a.s.Config.Map, Epoch: a.s.Config.Epoch, Player: steam, Name: stringField(player, "name"), Time: t + int64(a.s.Config.Delta*1000), Quality: "inferred", Uncertainty: a.s.Config.Uncertainty, Round: round + 1, Kills: after, GroupCount: diff, Evidence: "GSI " + field + " 状态差分；接收时间 " + time.Now().UTC().Format(time.RFC3339Nano)}
		for _, prior := range a.s.Events {
			if prior.Player == steam && prior.Round == e.Round && prior.Epoch == e.Epoch {
				e.Clutch = max(e.Clutch, prior.Clutch)
			}
		}
		if a.s.Config.Mode == "live" {
			e.RoundTiming = true
			e.Uncertainty = .15 // GSI countdown estimate; independent of legacy delay calibration.
			e.Time = 0
			e.Clock = readRoundClock(payload)
			if e.Clock != nil {
				e.Clock.Round = e.Round
			}
			e.LiveDuration = a.liveDuration[side]
			if !validRoundClock(e.Clock) {
				e.Clock = nil
			}
			// Last kill may arrive with the new phase; project from the preceding
			// active phase so B can start recording BEFORE the phase transition.
			beforeClock := readRoundClock(old)
			if beforeClock != nil {
				beforeClock.Round = previousRound
			}
			if validRoundClock(beforeClock) && (e.Clock == nil || e.Clock.Phase != beforeClock.Phase) && beforeClock.Round == e.Round {
				previousAt := a.gsiDiagnostics[side].AcceptedAt
				if t-previousAt <= 1500 {
					beforeClock.Remaining -= float64(t-previousAt) / 1000
					if validRoundClock(beforeClock) {
						e.Clock = beforeClock
					}
				}
			}
			if anchor, ok := a.phaseAnchors[side]; ok && e.Clock != nil && anchor.Clock.Round == e.Round && phaseOrder(anchor.Clock.Phase) < phaseOrder(e.Clock.Phase) {
				clock := anchor.Clock
				e.ClockAnchor = &clock
				e.ClockOffset = float64(t-anchor.At) / 1000
			}
		}
		if err := a.upsertLocked(e); err != nil {
			a.logLocked("warn", err.Error())
		} else {
			d.Detected++
		}
	}
	return nil
}

// A owns the director map; B owns the recording map. Delayed feeds cannot
// switch the other machine back to an old map.
func (a *Service) adoptMapLocked(gameMap string) {
	old := a.s.Config.Map
	a.s.Config.Map = gameMap
	a.s.Config.Epoch++
	a.s.Config.CalibratedUntil = 0
	a.s.Config.Paused = !a.s.Config.AutoCapture || a.localDemo.Phase == "armed" || a.cs2Launch.Phase == "starting"
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
	a.roundClocks = nil
	a.phaseAnchors = nil
	a.liveDuration = nil
	a.observed = ""
	a.observedAt = 0
	a.remoteAt = 0
	a.s.Events = Plan(a.s.Events, a.s.Jobs, a.s.Config, nowMS())
	a.logLocked("info", fmt.Sprintf("GSI 自动识别地图：%s → %s；已切换时间线并清空旧回放队列", old, gameMap))
	a.saveLocked()
}

// Use the pre-kill snapshot, so a 1v2 becoming 1v1 keeps its priority.
func clutchOpponents(players map[string]any, steam string) int {
	player, _ := players[steam].(map[string]any)
	team := stringField(player, "team")
	if team != "CT" && team != "T" {
		return 0
	}
	own, enemies := 0, 0
	for _, raw := range players {
		p, _ := raw.(map[string]any)
		health, known := gsiCount(p, "state", "health")
		if !known {
			return 0
		}
		if health == 0 {
			continue
		}
		side := stringField(p, "team")
		if side == team {
			own++
		} else if side == "CT" || side == "T" {
			enemies++
		}
	}
	if own == 1 && (enemies == 2 || enemies == 3) {
		return enemies
	}
	return 0
}

func (a *Service) gsiConfig(side, base string) string {
	return fmt.Sprintf("\"Project Replay %s\"\n{\n  \"uri\" \"%s/gsi/%s\"\n  \"timeout\" \"2.0\"\n  \"buffer\" \"0.05\"\n  \"throttle\" \"0.1\"\n  \"heartbeat\" \"1.0\"\n  \"data\" { \"provider\" \"1\" \"map\" \"1\" \"round\" \"1\" \"phase_countdowns\" \"1\" \"player_id\" \"1\" \"player_state\" \"1\" \"allplayers_id\" \"1\" \"allplayers_state\" \"1\" \"allplayers_match_stats\" \"1\" }\n}\n", side, base, side)
}
