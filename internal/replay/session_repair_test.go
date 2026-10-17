package replay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpiredDiagnosisSurvivesLaterRounds(t *testing.T) {
	a := testService(t, "agent")
	now := nowMS()
	e := clockEvent("lost", 5, "bomb", 20)
	e.Status = "CONFLICT"
	e.Reason = "视角冲突未录制"
	a.s.Events = []Event{e}
	a.roundClocks = map[string]clockSample{"b": {RoundClock{6, "live", 80}, now, now}}
	a.resolveRoundEventsLocked(now)
	reason := a.s.Events[0].Reason
	if !strings.Contains(reason, "视角冲突") {
		t.Fatal(reason)
	}
	a.roundClocks = nil
	a.resolveRoundEventsLocked(now + 2000)
	a.s.Events = Plan(a.s.Events, nil, a.s.Config, now+2000)
	if a.s.Events[0].Status != "MISSED_WINDOW" || a.s.Events[0].Reason != reason {
		t.Fatal(a.s.Events)
	}
}

func TestMappingYieldsToKnownUpcomingTarget(t *testing.T) {
	a := testService(t, "agent")
	a.refreshPlayerMappingLocked(false)
	a.playerMapping.Slots["A"] = 1
	now := nowMS()
	e := clockEvent("upcoming", 5, "live", 78)
	a.s.Events = []Event{e}
	a.roundClocks = map[string]clockSample{"b": {RoundClock{5, "live", 80}, now, now}}
	if !a.mappingWouldDelayCaptureLocked(now) {
		t.Fatal("mapping would block capture")
	}
	a.playerMapping.Slots = map[string]int{}
	if a.mappingWouldDelayCaptureLocked(now) {
		t.Fatal("unknown target prevents discovery")
	}
}

func TestMappingResumeKeepsVerifiedSlots(t *testing.T) {
	first := 0
	slots, err := discoverSlotsFrom([]string{"A", "B"}, func(slot int) (string, error) {
		if first == 0 {
			first = slot
		}
		if slot == 42 {
			return "B", nil
		}
		if slot == 1 {
			return "A", nil
		}
		return "", nil
	}, func(int) {}, 42, map[string]int{"A": 1})
	if err != nil || first != 42 || slots["A"] != 1 || slots["B"] != 42 {
		t.Fatal(slots, err, first)
	}
}

func TestCameraWaitsOnlyThroughExcludedGuard(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Guard = .2
	now := nowMS()
	a.observed = "old"
	a.observedAt = now
	m := &playerCamera{service: a, job: Job{Player: "new", Epoch: a.s.Config.Epoch, Start: now - 50}, switchAt: now - 100}
	if err := m.Check(); err != nil {
		t.Fatal("rejected settling guard", err)
	}
	m.job.Start = now - 300
	if err := m.Check(); err == nil {
		t.Fatal("accepted wrong target in usable footage")
	}
}

func TestRecordingStartupBudgetSeparateFromSwitch(t *testing.T) {
	c := Config{Setup: .2}
	if recordingSetupMS(c) != 300 || switchSetupMS(c) != 200 {
		t.Fatal("budgets not independent")
	}
}

func TestRuntimeHistoryOutlivesDashboard(t *testing.T) {
	a := testService(t, "agent")
	for i := 0; i < 205; i++ {
		a.logLocked("info", "audit marker")
	}
	b, err := os.ReadFile(filepath.Join(a.dir, "runtime.jsonl"))
	if err != nil || strings.Count(string(b), "audit marker") != 205 || len(a.s.Logs) != 200 {
		t.Fatal("lost log history", err)
	}
}

func TestGOTVReportsTimeoutAndStaleData(t *testing.T) {
	a := testService(t, "agent")
	a.gotvReconnect = CS2Launch{Phase: "sent"}
	a.gotvSentAt = nowMS() - 31000
	w := request(t, a, "GET", "/api/gotv-status", nil, "", "")
	if !strings.Contains(w.Body.String(), `"phase":"timeout"`) {
		t.Fatal(w.Body.String())
	}
	a.gotvReconnect.Phase = "receiving"
	a.gsiSeen["b"] = nowMS() - 6000
	w = request(t, a, "GET", "/api/gotv-status", nil, "", "")
	if !strings.Contains(w.Body.String(), `"phase":"stale"`) {
		t.Fatal(w.Body.String())
	}
}
