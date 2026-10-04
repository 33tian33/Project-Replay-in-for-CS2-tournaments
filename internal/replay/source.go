package replay

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"

	dem "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/msg"
)

// SourceOptions describes an explicit match/feed identity; SourceID must remain
// stable across retries. Times emitted by ParseSource are game-time milliseconds.
type SourceOptions struct {
	Match, Map, SourceID string
	Epoch                int
	FromTick, ToTick     int64
}
type sourceThrow struct {
	id, owner, kind string
	entity          int
	start, end      int64
	points          []TrackPoint
	effectCamera    *[3]float64
}
type sourceDeath struct {
	event      Event
	kind       string
	weapon     string
	victimUser int
	victimEye  *[3]float64
	victimPos  [3]float64
}

func utilityKind(t common.EquipmentType) string {
	switch t {
	case common.EqHE:
		return "hegrenade"
	case common.EqMolotov:
		return "molotov"
	case common.EqIncendiary:
		return "incgrenade"
	case common.EqFlash:
		return "flashbang"
	case common.EqSmoke:
		return "smokegrenade"
	case common.EqDecoy:
		return "decoy"
	}
	return ""
}
func ParseSource(ctx context.Context, r io.Reader, broadcast string, opt SourceOptions, emit func(Event) error) (err error) {
	// Parser failures stop ingestion. No partially parsed event is promoted to a kill.
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("demo parser panic: %v", v)
		}
	}()
	if !validID(opt.SourceID) || len(opt.SourceID) > 48 || opt.Match == "" || opt.Epoch < 1 {
		return fmt.Errorf("事件源必须提供稳定的 source-id、比赛和 epoch")
	}
	var p dem.Parser
	if broadcast != "" {
		p, err = dem.NewCSTVBroadcastParser(broadcast)
		if err != nil {
			return err
		}
	} else {
		p = dem.NewParser(r)
	}
	defer p.Close()
	mapName := opt.Map
	p.RegisterNetMessageHandler(func(h *msg.CDemoFileHeader) {
		if mapName == "" {
			mapName = h.GetMapName()
		}
	})
	throws := map[int64]*sourceThrow{}
	byEntity := map[int]*sourceThrow{}
	explosions := map[int]int64{}
	fires := map[*common.Inferno]*sourceThrow{}
	pending := []sourceDeath{}
	roundKills := map[string]int{}
	prevRound := -1
	sourceNow := func() int64 { return p.CurrentTime().Milliseconds() }
	p.RegisterEventHandler(func(e events.InfernoStart) {
		fires[e.Inferno] = &sourceThrow{id: fmt.Sprintf("%s-fire-%d-%d", opt.SourceID, p.GameState().IngameTick(), e.Inferno.Entity.ID()), start: sourceNow()}
	})
	p.RegisterEventHandler(func(e events.InfernoExpired) { delete(fires, e.Inferno) })
	p.RegisterEventHandler(func(e events.GrenadeProjectileThrow) {
		g := e.Projectile
		if g == nil || g.Thrower == nil || g.WeaponInstance == nil {
			return
		}
		owner := strconv.FormatUint(g.Thrower.SteamID64, 10)
		kind := utilityKind(g.WeaponInstance.Type)
		if kind == "" {
			return
		}
		t := &sourceThrow{id: fmt.Sprintf("%s-%d-%d", opt.SourceID, p.GameState().IngameTick(), g.Entity.ID()), owner: owner, kind: kind, entity: g.Entity.ID(), start: sourceNow()}
		throws[g.UniqueID()] = t
		byEntity[g.Entity.ID()] = t
	})
	addPoint := func(t *sourceThrow, g *common.GrenadeProjectile) {
		now := sourceNow()
		v := g.Position()
		pt := TrackPoint{now, [3]float64{v.X, v.Y, v.Z}, "projectile"}
		if len(t.points) > 0 && t.points[len(t.points)-1].Time == now {
			t.points[len(t.points)-1] = pt
			return
		}
		t.points = append(t.points, pt)
	}
	p.RegisterEventHandler(func(e events.GrenadeProjectileDestroy) {
		if t := throws[e.Projectile.UniqueID()]; t != nil {
			addPoint(t, e.Projectile)
			t.end = sourceNow()
		}
	})
	p.RegisterEventHandler(func(e events.HeExplode) { explosions[e.GrenadeEntityID] = sourceNow() })
	p.RegisterEventHandler(func(e events.Kill) {
		if e.Killer == nil || e.Victim == nil || e.Weapon == nil || p.GameState().IsWarmupPeriod() {
			return
		}
		tick := int64(p.GameState().IngameTick())
		round := p.GameState().TotalRoundsPlayed() + 1
		if round != prevRound {
			roundKills = map[string]int{}
			prevRound = round
		}
		owner := strconv.FormatUint(e.Killer.SteamID64, 10)
		roundKills[owner]++
		if tick < opt.FromTick || (opt.ToTick > 0 && tick > opt.ToTick) {
			return
		}
		victim := strconv.FormatUint(e.Victim.SteamID64, 10)
		v := e.Victim.Position()
		event := Event{ID: fmt.Sprintf("%s-%d-%s", opt.SourceID, tick, victim), Match: opt.Match, Map: mapName, Epoch: opt.Epoch, Player: owner, Victim: victim, Name: e.Killer.Name, Time: sourceNow(), Tick: &tick, TickDomain: "server", Quality: "verified", Evidence: "demoinfocs v5.2.0 player_death", Round: round, Kills: roundKills[owner], GroupCount: 1}
		var eye *[3]float64
		if x, ok := e.Victim.PositionEyes(); ok {
			eye = &[3]float64{x.X, x.Y, x.Z}
		}
		pending = append(pending, sourceDeath{event, utilityKind(e.Weapon.Type), e.Weapon.OriginalString, e.Victim.UserID, eye, [3]float64{v.X, v.Y, v.Z}})
	})
	// EquipmentType collapses molotov impact and inferno into EqMolotov.
	// Preserve the actual player_death weapon before resolving its camera target.
	p.RegisterEventHandler(func(e events.GenericGameEvent) {
		if e.Name != "player_death" {
			return
		}
		uid := int(e.Data["userid"].GetValShort())
		for i := range pending {
			if pending[i].event.Time == sourceNow() && pending[i].victimUser&255 == uid&255 {
				pending[i].weapon = e.Data["weapon"].GetValString()
			}
		}
	})
	var callbackErr error
	p.RegisterEventHandler(func(events.FrameDone) {
		if callbackErr != nil {
			return
		}
		now := sourceNow()
		for _, g := range p.GameState().GrenadeProjectiles() {
			if t := throws[g.UniqueID()]; t != nil {
				addPoint(t, g)
			}
		}
		for _, d := range pending {
			e := d.event
			if d.kind != "" {
				candidates := []*sourceThrow{}
				if d.kind == "hegrenade" {
					for entity, at := range explosions {
						if abs64(at-e.Time) <= 50 {
							if t := byEntity[entity]; t != nil && t.owner == e.Player && t.kind == d.kind {
								candidates = append(candidates, t)
							}
						}
					}
				} else if d.weapon == "inferno" {
					// Only a unique active fire owned by the attacker and touching the victim
					// can resolve attribution. Overlapping matching fires stay unresolved.
					for _, inf := range p.GameState().Infernos() {
						fire := fires[inf]
						if fire == nil {
							continue
						}
						owner := inf.Thrower()
						if owner == nil || strconv.FormatUint(owner.SteamID64, 10) != e.Player {
							continue
						}
						close := false
						focus := [3]float64{}
						bestDistance := math.Inf(1)
						for _, f := range inf.Fires().Active().List() {
							// Approximate flame proximity to the player's 32-unit hull,
							// not just its centre. Ambiguous fires are never resolved.
							dx := math.Max(math.Abs(f.X-d.victimPos[0])-16, 0)
							dy := math.Max(math.Abs(f.Y-d.victimPos[1])-16, 0)
							dz := f.Z - d.victimPos[2]
							if dx*dx+dy*dy < 60*60 && math.Abs(dz) < 100 {
								close = true
								if distance := dx*dx + dy*dy + dz*dz; distance < bestDistance {
									bestDistance = distance
									focus = [3]float64{f.X, f.Y, f.Z}
								}
							}
						}
						if !close {
							continue
						}
						points := []TrackPoint{{max(fire.start, e.Time-1500), focus, "effect"}, {e.Time + 1500, focus, "effect"}}
						candidates = append(candidates, &sourceThrow{id: fire.id, owner: e.Player, kind: d.kind, points: points, effectCamera: d.victimEye})
					}
				} else {
					for _, t := range throws {
						if t.owner == e.Player && t.kind == d.kind && ((t.end > 0 && abs64(t.end-e.Time) <= 50) || projectileImpact(t, e.Time, d.victimPos)) {
							candidates = append(candidates, t)
						}
					}
				}
				e.Utility = &Utility{Kind: d.kind}
				if len(candidates) == 1 {
					t := candidates[0]
					points := append([]TrackPoint(nil), t.points...)
					// Keep 2 seconds of measured flight before the kill, enough for guard.
					i := 0
					for i+1 < len(points) && points[i+1].Time < e.Time-2000 {
						i++
					}
					points = points[i:]
					if len(points) > 0 && points[len(points)-1].Phase == "projectile" {
						last := points[len(points)-1]
						at := max(e.Time, last.Time+1)
						points = append(points, TrackPoint{at, last.Position, "effect"}, TrackPoint{e.Time + 1500, last.Position, "effect"})
					}
					e.Utility = &Utility{Kind: t.kind, ID: t.id, Track: points, EffectCamera: t.effectCamera, Evidence: fmt.Sprintf("%s: unique owner/type/effect association; source tick %d; fire/impact proximity is estimated", opt.SourceID, *e.Tick)}
					if err := validateUtility(e.Utility); err != nil {
						e.Utility = &Utility{Kind: d.kind}
						e.Evidence += "; unresolved trajectory: " + err.Error()
					}
				} else {
					e.Evidence += fmt.Sprintf("; utility association candidates=%d", len(candidates))
				}
			}
			if callbackErr = emit(e); callbackErr != nil {
				return
			}
		}
		pending = nil
		for uid, t := range throws {
			if now-t.start > 60000 {
				delete(throws, uid)
				if byEntity[t.entity] == t {
					delete(byEntity, t.entity)
				}
			}
		}
		for id, at := range explosions {
			if now-at > 1000 {
				delete(explosions, id)
			}
		}
	})
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		more, e := p.ParseNextFrame()
		if e != nil {
			return e
		}
		if callbackErr != nil {
			return callbackErr
		}
		if !more || (opt.ToTick > 0 && int64(p.GameState().IngameTick()) > opt.ToTick+2) {
			return nil
		}
	}
}

// Direct grenade impact may kill before the projectile is destroyed. Require a
// recent sample at the victim's collision hull, not an arbitrary nearby throw.
func projectileImpact(t *sourceThrow, at int64, victim [3]float64) bool {
	if len(t.points) == 0 || t.end != 0 {
		return false
	}
	p := t.points[len(t.points)-1]
	if abs64(p.Time-at) > 50 {
		return false
	}
	dx := math.Max(math.Abs(p.Position[0]-victim[0])-16, 0)
	dy := math.Max(math.Abs(p.Position[1]-victim[1])-16, 0)
	dz := math.Max(math.Max(victim[2]-p.Position[2], p.Position[2]-victim[2]-72), 0)
	return dx*dx+dy*dy+dz*dz <= 8*8
}
func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// MapSourceEvent applies one explicit source-game-time -> wall-time anchor.
func MapSourceEvent(e Event, offset int64, uncertainty float64) Event {
	e = jsonCopy(e)
	e.Time += offset
	shiftUtility(e.Utility, offset)
	e.Uncertainty = uncertainty
	return e
}

// SortSourceEvents is useful for file-based integrations; live feeds retain order.
func SortSourceEvents(es []Event) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].Time != es[j].Time {
			return es[i].Time < es[j].Time
		}
		return es[i].ID < es[j].ID
	})
}
