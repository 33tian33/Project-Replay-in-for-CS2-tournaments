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
