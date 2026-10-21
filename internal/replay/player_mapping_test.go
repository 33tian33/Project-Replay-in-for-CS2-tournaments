package replay

import (
	"bufio"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoverSlotsRequiresRepeatableChanges(t *testing.T) {
	for _, initial := range []string{"outside", "A"} {
		t.Run(initial, func(t *testing.T) {
			current := initial
			slots := map[int]string{1: "A", 14: "B", 29: "C"}
			result, err := discoverSlots([]string{"A", "B", "C"}, func(slot int) (string, error) {
				next := slots[slot]
				if next == "" || next == current {
					return "", nil
				}
				current = next
				return next, nil
			}, func(int) {})
			if err != nil || !reflect.DeepEqual(result, map[string]int{"A": 1, "B": 14, "C": 29}) {
				t.Fatal(result, err)
			}
		})
	}
	// One late/spurious change from an invalid command must not be accepted.
	calls := 0
	result, err := discoverSlots([]string{"A", "B"}, func(slot int) (string, error) {
		calls++
		if calls == 1 {
			return "A", nil
		}
		if calls == 2 {
			return "B", nil
		}
		return "", nil
	}, func(int) {})
	if err != nil || len(result) != 0 {
		t.Fatal("accepted unrepeatable mapping", result, err)
	}
}

func TestAutomaticMappingInvalidationAndManualOverride(t *testing.T) {
	a := testService(t, "agent")
	a.previous["b"] = map[string]any{"allplayers": map[string]any{"76561198000000001": map[string]any{}}}
	a.refreshPlayerMappingLocked(false)
	old := a.playerMapping
	old.Slots["76561198000000001"] = 14
	if got := a.captureConfigLocked().Mappings["76561198000000001"]; got != 14 {
		t.Fatal(got)
	}
	if len(a.s.Config.Mappings) != 0 {
		t.Fatal("automatic slots persisted in manual configuration")
	}
	a.s.Config.Mappings["76561198000000001"] = 29
	if got := a.captureConfigLocked().Mappings["76561198000000001"]; got != 29 {
		t.Fatal(got)
	}
	a.s.Config.Mappings = map[string]int{}
	a.s.Config.Epoch++
	if len(a.captureConfigLocked().Mappings) != 0 || a.playerMapping == old {
		t.Fatal("old epoch mapping survived")
	}
	a.playerMapping.Slots["76561198000000001"] = 14
	a.refreshPlayerMappingLocked(true)
	if len(a.captureConfigLocked().Mappings) != 0 {
		t.Fatal("outage mapping survived")
	}
	a.playerMapping.Slots["76561198000000001"] = 14
	obj(a.previous["b"], "allplayers")["76561198000000002"] = map[string]any{}
	if len(a.captureConfigLocked().Mappings) != 0 {
		t.Fatal("roster change mapping survived")
	}
}

func TestAutomaticMappingFromConsoleAndGSI(t *testing.T) {
	a := testService(t, "agent")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ids := []string{"76561198000000001", "76561198000000002"}
	var target atomic.Int32
	target.Store(-1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scan := bufio.NewScanner(conn)
		for scan.Scan() {
			line := scan.Text()
			if strings.HasPrefix(line, "spec_player ") {
				var slot int
				fmt.Sscanf(line, "spec_player %d", &slot)
				if slot >= 1 && slot <= 2 {
					target.Store(int32(slot - 1))
				}
			}
			if strings.HasPrefix(line, "echo ") {
				fmt.Fprintln(conn, strings.TrimPrefix(line, "echo "))
			}
		}
	}()
	a.s.Config.Mode = "live"
	a.s.Config.NetCon = listener.Addr().String()
	a.s.Config.SessionLock = filepath.Join(t.TempDir(), "session.lock")
	send := func() {
		p := clockGSI(1, 0, "live", 90)
		p["allplayers"] = map[string]any{ids[0]: map[string]any{"name": "5hr A"}, ids[1]: map[string]any{"name": "5hr B"}}
		observed := "76561198000000003"
		if i := target.Load(); i >= 0 {
			observed = ids[i]
		}
		p["player"] = map[string]any{"steamid": observed}
		a.ingestGSI("b", p)
	}
	send()
	go func() {
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				send()
			}
		}
	}()
	a.mu.Lock()
	started := a.startPlayerMappingLocked(nowMS())
	a.mu.Unlock()
	if !started {
		t.Fatal("did not automatically start")
	}
	for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline); {
		a.mu.Lock()
		busy := a.mappingBusy
		state := jsonCopy(a.playerMapping)
		c := a.captureConfigLocked()
		a.mu.Unlock()
		if !busy {
			if state.Phase != "ready" || c.Mappings[ids[0]] != 1 || c.Mappings[ids[1]] != 2 {
				t.Fatal(state, c.Mappings)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("automatic discovery timed out")
}

func TestAutomaticMappingDoesNotTakeCommittedCamera(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	a.previous["b"] = map[string]any{"allplayers": map[string]any{"76561198000000001": map[string]any{}, "76561198000000002": map[string]any{}}}
	a.observed = "76561198000000001"
	a.gsiSeen["b"] = nowMS()
	a.s.Jobs = []Job{{Status: "COMMITTED"}}
	a.mu.Lock()
	started := a.startPlayerMappingLocked(nowMS())
	a.mu.Unlock()
	if started || a.mappingBusy {
		t.Fatal("mapping stole a committed camera")
	}
	a.s.Jobs = nil
	a.captureMu.Lock()
	a.mu.Lock()
	started = a.startPlayerMappingLocked(nowMS())
	a.mu.Unlock()
	a.captureMu.Unlock()
	if started || a.mappingBusy {
		t.Fatal("mapping stole camera mutex")
	}
}

func TestAutomaticMappingGSIOutageInvalidatesSlots(t *testing.T) {
	a := testService(t, "agent")
	p := clockGSI(1, 0, "live", 90)
	p["allplayers"] = map[string]any{"76561198000000001": map[string]any{}}
	a.ingestGSI("b", p)
	a.playerMapping.Slots["76561198000000001"] = 14
	before := a.playerMapping
	a.gsiSeen["b"] = nowMS() - 6000
	a.ingestGSI("b", p)
	if a.playerMapping == before || len(a.playerMapping.Slots) != 0 {
		t.Fatal("outage preserved old slots")
	}
}

func TestAgentPlanningWaitsForAutomaticMapping(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	c := a.captureConfigLocked()
	e := clockEvent("event", 1, "live", 90)
	e.Time = nowMS() + 5000
	got := Plan([]Event{e}, nil, c, nowMS())
	if got[0].Status != "IDENTITY_UNKNOWN" {
		t.Fatal(got)
	}
	c.Mappings[e.Player] = 14
	got = Plan([]Event{e}, nil, c, nowMS())
	if got[0].Status != "SCHEDULED" {
		t.Fatal(got)
	}
}

func TestWrongTargetInvalidatesAutomaticSlot(t *testing.T) {
	a := testService(t, "agent")
	a.refreshPlayerMappingLocked(false)
	a.playerMapping.Slots["A"] = 14
	now := nowMS()
	a.observed = "B"
	a.observedAt = now - 10
	m := &playerCamera{service: a, job: Job{Player: "A", Slot: 14, Epoch: a.s.Config.Epoch, Start: now - 1000}, switchAt: now - 50}
	if m.Check() == nil || a.playerMapping.Slots["A"] != 0 {
		t.Fatal("bad automatic slot retained")
	}
	a.s.Config.Mappings["A"] = 14
	a.playerMapping.Slots["A"] = 14
	if m.Check() == nil || a.s.Config.Mappings["A"] != 14 || a.playerMapping.Slots["A"] != 14 {
		t.Fatal("manual override modified")
	}
}

func TestMappingNormalYieldHasNoCooldown(t *testing.T) {
	if mappingRetryDelay(nil) != 0 || mappingRetryDelay(errMappingYield) != 0 {
		t.Fatal("normal continuation must not wait")
	}
	if mappingRetryDelay(errMappingIncomplete) > 1000 || mappingRetryDelay(fmt.Errorf("console unavailable")) != 5000 {
		t.Fatal("incomplete scan and transport failure need different backoff")
	}
}

func TestMappingCandidateSurvivesSliceAndIsReverified(t *testing.T) {
	d := &slotDiscovery{Next: 14, Verified: map[string]int{"A": 1}, Seen: map[int]bool{}}
	current := "A"
	calls := 0
	probe := func(slot int) (string, error) {
		calls++
		if calls == 3 {
			return "", errMappingYield
		}
		next := map[int]string{1: "A", 14: "B"}[slot]
		if next == "" || next == current {
			return "", nil
		}
		current = next
		return next, nil
	}
	if err := d.discover([]string{"A", "B"}, probe, func() {}); err != errMappingYield {
		t.Fatal(err)
	}
	if d.Candidates["B"] != 14 || d.Verified["B"] != 0 || d.Next != 15 {
		t.Fatal(d)
	}
	// Resume from the saved candidate, without traversing slots 15..128 first.
	calls = 100
	if err := d.discover([]string{"A", "B"}, probe, func() {}); err != nil {
		t.Fatal(err)
	}
	if d.Verified["B"] != 14 || calls > 106 {
		t.Fatal(d, calls)
	}
}

func TestMappingInterruptedProbeDoesNotSkipSlot(t *testing.T) {
	d := &slotDiscovery{Next: 42}
	err := d.discover([]string{"A", "B"}, func(int) (string, error) { return "", errMappingYield }, func() {})
	if err != errMappingYield || d.Next != 42 {
		t.Fatal(d, err)
	}
}

func TestMappingHintCannotBecomeCaptureMappingWithoutChanges(t *testing.T) {
	d := &slotDiscovery{Next: 1, Candidates: map[string]int{"A": 1, "B": 14}}
	if err := d.discover([]string{"A", "B"}, func(int) (string, error) { return "", nil }, func() {}); err != nil {
		t.Fatal(err)
	}
	if len(d.Verified) != 0 {
		t.Fatal("no-op hints accepted", d)
	}
}

func TestMappingRosterRefreshPreservesOnlyHints(t *testing.T) {
	a := testService(t, "agent")
	const aID = "76561198000000001"
	const bID = "76561198000000002"
	a.previous["b"] = map[string]any{"allplayers": map[string]any{aID: map[string]any{}}}
	a.refreshPlayerMappingLocked(false)
	a.playerMapping.Slots[aID] = 14
	obj(a.previous["b"], "allplayers")[bID] = map[string]any{}
	a.refreshPlayerMappingLocked(false)
	if a.playerMapping.candidates[aID] != 14 || len(a.captureConfigLocked().Mappings) != 0 {
		t.Fatal(a.playerMapping)
	}
	a.s.Config.Epoch++
	a.refreshPlayerMappingLocked(false)
	if len(a.playerMapping.candidates) != 0 {
		t.Fatal("hint crossed sessions")
	}
}

func TestMappingNormalRoundRetainsVerifiedIdentity(t *testing.T) {
	a := testService(t, "agent")
	const player = "76561198000000001"
	p := clockGSI(1, 0, "live", 90)
	p["allplayers"] = map[string]any{player: map[string]any{}}
	if err := a.ingestGSI("b", p); err != nil {
		t.Fatal(err)
	}
	a.playerMapping.Slots[player] = 14
	old := a.playerMapping
	p = clockGSI(2, 0, "freezetime", 15)
	p["allplayers"] = map[string]any{player: map[string]any{}}
	if err := a.ingestGSI("b", p); err != nil {
		t.Fatal(err)
	}
	if a.playerMapping != old || a.captureConfigLocked().Mappings[player] != 14 {
		t.Fatal("normal round cleared mapping")
	}
}

func TestMappingPrioritizesKnownNeighborsBeforeEmptyTail(t *testing.T) {
	d := &slotDiscovery{Next: 80, Verified: map[string]int{"A": 14}}
	current := "A"
	probes := []int{}
	err := d.discover([]string{"A", "B"}, func(slot int) (string, error) {
		probes = append(probes, slot)
		next := map[int]string{14: "A", 15: "B"}[slot]
		if next == "" || next == current {
			return "", nil
		}
		current = next
		return next, nil
	}, func() {})
	if err != nil || d.Verified["B"] != 15 {
		t.Fatal(d, err)
	}
	for _, slot := range probes {
		if slot >= 80 {
			t.Fatal("scanned empty tail before neighbor", probes)
		}
	}
	if len(probes) > 8 {
		t.Fatal("too many probes", probes)
	}
}

func TestMappingPriorityQueueSurvivesInterruptedProbe(t *testing.T) {
	d := &slotDiscovery{Next: 80, Verified: map[string]int{"A": 14}}
	first := 0
	if err := d.discover([]string{"A", "B"}, func(slot int) (string, error) { first = slot; return "", errMappingYield }, func() {}); err != errMappingYield {
		t.Fatal(err)
	}
	second := 0
	d.discover([]string{"A", "B"}, func(slot int) (string, error) { second = slot; return "", errMappingYield }, func() {})
	if first != 13 || second != 13 || d.Next != 80 {
		t.Fatal(first, second, d)
	}
}

func TestMappingFullSweepProgressAcrossSlices(t *testing.T) {
	d := &slotDiscovery{Next: 1}
	calls := 0
	completed := false
	for slice := 0; slice < 30; slice++ {
		budget := 10
		err := d.discover([]string{"A", "B"}, func(int) (string, error) {
			if budget == 0 {
				return "", errMappingYield
			}
			budget--
			calls++
			return "", nil
		}, func() {})
		if err == nil {
			completed = true
			break
		}
		if err != errMappingYield {
			t.Fatal(err)
		}
	}
	if !completed || calls != 256 {
		t.Fatal("scan never finishes across slices", calls, d)
	}
}

func TestMappingOutageReverifiesHintsWithoutFullScan(t *testing.T) {
	a := testService(t, "agent")
	const one = "76561198000000001"
	const two = "76561198000000002"
	a.previous["b"] = map[string]any{"allplayers": map[string]any{one: map[string]any{}, two: map[string]any{}}}
	a.refreshPlayerMappingLocked(false)
	a.playerMapping.Slots = map[string]int{one: 90, two: 110}
	a.refreshPlayerMappingLocked(true)
	if len(a.captureConfigLocked().Mappings) != 0 {
		t.Fatal("outage hints trusted without verification")
	}
	d := &slotDiscovery{Candidates: jsonCopy(a.playerMapping.candidates)}
	current := one
	calls := 0
	if err := d.discover([]string{one, two}, func(slot int) (string, error) {
		calls++
		if slot != 90 && slot != 110 {
			t.Fatalf("blind scanned %d instead of rechecking hints", slot)
		}
		next := map[int]string{90: one, 110: two}[slot]
		if current == next {
			return "", nil
		}
		current = next
		return next, nil
	}, func() {}); err != nil {
		t.Fatal(err)
	}
	if len(d.Verified) != 2 || calls > 5 {
		t.Fatal(d, calls)
	}
	// Temporary roster omissions keep history only, never capture eligibility.
	a.playerMapping.Slots = d.Verified
	delete(obj(a.previous["b"], "allplayers"), two)
	a.refreshPlayerMappingLocked(false)
	obj(a.previous["b"], "allplayers")[two] = map[string]any{}
	a.refreshPlayerMappingLocked(false)
	if a.playerMapping.candidates[two] != 110 || len(a.captureConfigLocked().Mappings) != 0 {
		t.Fatal(a.playerMapping)
	}
}

func TestMappingDoesNotBlockPlanningKnownPlayer(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	a.s.Config.Setup = .3
	a.s.Config.Guard = .2
	const known = "76561198000000001"
	const unknown = "76561198000000002"
	a.previous["b"] = map[string]any{"allplayers": map[string]any{known: map[string]any{}, unknown: map[string]any{}}}
	a.refreshPlayerMappingLocked(false)
	a.playerMapping.Slots[known] = 14
	now := nowMS()
	a.mappingBusy = true
	a.playerMapping.deadline = now + 6000
	e := clockEvent("known", 5, "live", 78.2)
	e.Player = known
	missing := clockEvent("missing", 5, "live", 75)
	missing.Player = unknown
	a.s.Events = []Event{e, missing}
	a.roundClocks = map[string]clockSample{"b": {RoundClock{5, "live", 80}, now, now}}
	a.tick()
	if a.s.Events[0].Status != "SCHEDULED" || a.s.Events[1].Status != "IDENTITY_UNKNOWN" {
		t.Fatal(a.s.Events)
	}
	if a.playerMapping.deadline > nowMS() || len(a.s.Jobs) != 0 {
		t.Fatal("did not yield before committing", a.playerMapping, a.s.Jobs)
	}
	a.mappingBusy = false
	a.tick()
	a.cancel() // Prevent the synthetic committed job from reaching real adapters.
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.s.Jobs) != 1 || a.s.Jobs[0].Player != known || a.s.Jobs[0].Slot != 14 {
		t.Fatal(a.s.Jobs)
	}
}

func TestMappingYieldsOnlyToValidatedRemoteJob(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	a.s.Config.CalibratedUntil = nowMS() + 60000
	a.s.Config.Paused = false
	const known = "76561198000000001"
	a.previous["b"] = map[string]any{"allplayers": map[string]any{known: map[string]any{}}}
	a.refreshPlayerMappingLocked(false)
	a.playerMapping.Slots[known] = 14
	a.mappingBusy = true
	deadline := nowMS() + 6000
	a.playerMapping.deadline = deadline
	start := nowMS() + 10000
	e := ev("remoteevent", known, start+1200)
	j := Job{ID: "remotejob", Epoch: e.Epoch, Match: e.Match, Map: e.Map, Player: known, Slot: 13, Start: start, End: start + 2400, Events: []Event{e}}
	if _, err := a.acceptJob(j); err == nil || a.playerMapping.deadline != deadline {
		t.Fatal("invalid job preempted scan", err)
	}
	j.Slot = 14
	got, err := a.acceptJob(j)
	if err != nil || got.Status != "COMMITTED" || a.playerMapping.deadline > nowMS() {
		t.Fatal(got, err)
	}
	a.cancel()
}
