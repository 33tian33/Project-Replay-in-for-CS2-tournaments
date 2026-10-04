package replay

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOpeningKillSurvivesRoundCounterReset(t *testing.T) {
	a := testService(t, "director")
	a.s.Config.Mode = "live"
	old := clockGSI(2, 10, "over", 1)
	obj(obj(old, "allplayers"), "A")["state"] = map[string]any{"round_kills": 3.0}
	next := clockGSI(3, 11, "live", 114.5)
	obj(obj(next, "allplayers"), "A")["state"] = map[string]any{"round_kills": 1.0}
	a.ingestGSI("a", old)
	a.ingestGSI("a", next)
	if len(a.s.Events) != 1 || a.s.Events[0].Round != 3 || a.s.Events[0].Kills != 1 || a.s.Events[0].Clock.Phase != "live" {
		t.Fatal(a.s.Events)
	}
	a.ingestGSI("a", next)
	if len(a.s.Events) != 1 {
		t.Fatal("duplicate opening kill")
	}
}
func TestBriefGSIOutagePreservesPendingRoundEvents(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	a.ingestGSI("b", clockGSI(5, 0, "live", 90))
	epoch := a.s.Config.Epoch
	e := clockEvent("pending", 5, "live", 70)
	e.Epoch = epoch
	a.s.Events = []Event{e}
	a.gsiSeen["b"] = nowMS() - 6000
	a.ingestGSI("b", clockGSI(5, 0, "live", 84))
	if a.s.Config.Epoch != epoch {
		t.Fatal("short outage discarded queued kills")
	}
	a.ingestGSI("b", clockGSI(4, 0, "live", 90))
	if a.s.Config.Epoch == epoch {
		t.Fatal("actual rewind did not invalidate timeline")
	}
}
func TestOpeningKillPreparesDuringFreeze(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	now := nowMS()
	e := clockEvent("opening", 3, "live", 114.6)
	e.LiveDuration = 115
	a.s.Events = []Event{e}
	a.roundClocks = map[string]clockSample{"b": {RoundClock{3, "freezetime", 2}, now, now}}
	a.resolveRoundEventsLocked(now)
	if delta := a.s.Events[0].Time - now; delta < 2399 || delta > 2401 {
		t.Fatal(a.s.Events)
	}
	planned := Plan(a.s.Events, nil, a.s.Config, now)
	if planned[0].Status != "SCHEDULED" {
		t.Fatal(planned)
	}
	j := Job{Events: planned}
	if err := a.checkRoundJobLocked(j); err != nil {
		t.Fatal(err)
	}
	a.roundClocks["b"] = clockSample{RoundClock{3, "freezetime", 5}, now, now}
	if err := a.checkRoundJobLocked(j); err == nil {
		t.Fatal("pause shift accepted")
	}
}
func TestLiveDurationMeasuredFromBoundary(t *testing.T) {
	a := testService(t, "director")
	a.s.Config.Mode = "live"
	now := nowMS()
	a.roundClocks = map[string]clockSample{"a": {RoundClock{3, "freezetime", .1}, now - 200, now - 200}}
	a.updateRoundClockLocked("a", clockGSI(3, 0, "live", 114.9), now)
	if a.liveDuration["a"] < 114.99 || a.liveDuration["a"] > 115.01 {
		t.Fatal(a.liveDuration)
	}
}
func TestLateSetupStillPlansCompleteWindow(t *testing.T) {
	c := testConfig()
	c.Setup = .5
	e := ev("still-possible", "A", 2000)
	if got := Plan([]Event{e}, nil, c, 700); got[0].Status != "SCHEDULED" {
		t.Fatal(got)
	}
	if got := Plan([]Event{e}, nil, c, 801); got[0].Status != "MISSED_WINDOW" {
		t.Fatal(got)
	}
}
func TestLateSamePlayerKillExtendsOpenRecording(t *testing.T) {
	for _, status := range []string{"COMMITTED", "CAPTURING", "STOPPING", "FINALIZING"} {
		t.Run(status, func(t *testing.T) {
			a := testService(t, "agent")
			c := a.s.Config
			now := nowMS()
			first := ev("first", "A", now+1500)
			next := ev("next", "A", now+2400)
			start, end := bounds(first, c)
			a.s.Jobs = []Job{{ID: "recording", Epoch: c.Epoch, Player: "A", Events: []Event{first}, Start: start, End: end, Status: status}}
			a.s.Events = []Event{next}
			a.extendCapturesLocked(c, now)
			want := status == "COMMITTED" || status == "CAPTURING"
			if (a.s.Events[0].JobID != "") != want {
				t.Fatal(a.s.Events)
			}
			if want && (len(a.s.Jobs[0].Events) != 2 || a.s.Jobs[0].End <= end) {
				t.Fatal(a.s.Jobs)
			}
		})
	}
}
func TestExtensionDoesNotStealAnotherReservation(t *testing.T) {
	a := testService(t, "agent")
	c := a.s.Config
	now := nowMS()
	first := ev("first", "A", now+1500)
	next := ev("next", "A", now+2400)
	start, end := bounds(first, c)
	a.s.Jobs = []Job{{ID: "one", Epoch: c.Epoch, Events: []Event{first}, Start: start, End: end, Status: "CAPTURING"}, {ID: "two", Start: end + 500, End: end + 3000, Status: "COMMITTED"}}
	a.s.Events = []Event{next}
	a.extendCapturesLocked(c, now)
	if a.s.Events[0].JobID != "" {
		t.Fatal("extended into another camera reservation")
	}
}
func TestTeamsSyncAndHalftimeHUD(t *testing.T) {
	worker := testService(t, "agent")
	server := httptest.NewServer(worker.Handler())
	defer server.Close()
	director := testService(t, "director")
	director.s.Config.WorkerURL = server.URL
	teams := TeamSettings{CT: TeamIdentity{Name: "Alpha"}, T: TeamIdentity{Name: "Bravo"}, HalfRounds: 12, OvertimeHalfRounds: 3}
	epoch := worker.s.Config.Epoch
	if w := request(t, director, "POST", "/api/teams", teams, "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if worker.s.Config.Teams != teams || worker.s.Config.Epoch != epoch {
		t.Fatal("team sync changed timeline or lost settings")
	}
	for _, test := range []struct {
		round int
		ct    string
	}{{1, "Alpha"}, {12, "Alpha"}, {13, "Bravo"}, {24, "Bravo"}, {25, "Bravo"}, {28, "Alpha"}, {31, "Bravo"}} {
		worker.gsiRound["b"] = test.round - 1
		w := request(t, worker, "GET", "/hud-api/state", nil, "", "")
		var body struct {
			CT TeamIdentity `json:"ct"`
		}
		json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != 200 || body.CT.Name != test.ct {
			t.Fatal(test, w.Body.String())
		}
	}
}
func TestCleanupPairPreservesSettingsAndOwnedAssets(t *testing.T) {
	worker := testService(t, "agent")
	server := httptest.NewServer(worker.Handler())
	defer server.Close()
	director := testService(t, "director")
	director.s.Config.WorkerURL = server.URL
	for _, a := range []*Service{worker, director} {
		a.s.Config.Teams = TeamSettings{CT: TeamIdentity{Name: "Keep"}}
		a.s.Output.Transition1 = "keep.webm"
		for _, name := range []string{"keep.webm", "clip.mp4", "clip.mp4.part", "old-transition.webm"} {
			if err := os.WriteFile(filepath.Join(a.dir, "media", name), []byte("test"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		a.s.Events = []Event{ev("old", "A", nowMS())}
		raw := filepath.Join(t.TempDir(), "owned.mkv")
		os.WriteFile(raw, []byte("raw"), 0600)
		a.rememberRaw(raw)
	}
	before := director.s.Config
	output := director.s.Output
	w := request(t, director, "POST", "/api/cleanup", map[string]any{}, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if !reflect.DeepEqual(before, director.s.Config) || output != director.s.Output {
		t.Fatal("configuration changed")
	}
	for _, a := range []*Service{worker, director} {
		entries, _ := os.ReadDir(filepath.Join(a.dir, "media"))
		if len(entries) != 1 || entries[0].Name() != "keep.webm" || len(a.s.Events) != 0 {
			t.Fatal(entries, a.s.Events)
		}
	}
	// Idempotent retry.
	if w = request(t, director, "POST", "/api/cleanup", map[string]any{}, "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
func TestCleanupRemoteBusyLeavesLocalUntouched(t *testing.T) {
	worker := testService(t, "agent")
	worker.active = true
	server := httptest.NewServer(worker.Handler())
	defer server.Close()
	director := testService(t, "director")
	director.s.Config.WorkerURL = server.URL
	path := filepath.Join(director.dir, "media", "keep.mp4")
	os.WriteFile(path, []byte("keep"), 0600)
	if w := request(t, director, "POST", "/api/cleanup", map[string]any{}, "", ""); w.Code == 200 {
		t.Fatal("busy remote cleared")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("local deleted before remote succeeded")
	}
}
func TestPeerOperationsRejectDirector(t *testing.T) {
	a := testService(t, "director")
	for _, path := range []string{"/api/cleanup?agent=1", "/api/teams?agent=1", "/api/timing/compact?agent=1"} {
		if w := request(t, a, "POST", path, map[string]any{}, "", ""); w.Code != 400 {
			t.Fatal(path, w.Code)
		}
	}
}
func TestHUDValueReadback(t *testing.T) {
	for _, line := range []string{`cl_drawhud = true`, `"cl_drawhud" = "1"`, `cl_drawhud true (default true)`} {
		if value, ok := hudValue(line, "cl_drawhud"); !value || !ok {
			t.Fatal(line)
		}
	}
	if _, ok := hudValue("Can't use cheat command cl_drawhud", "cl_drawhud"); ok {
		t.Fatal("error mistaken for success")
	}
}

func TestFreezeLiveBoundaryKeepsCaptureClockRunning(t *testing.T) {
	a := testService(t, "agent")
	now := nowMS()
	a.roundClocks = map[string]clockSample{"b": {RoundClock{3, "freezetime", .1}, now - 200, now - 200}}
	a.updateRoundClockLocked("b", clockGSI(3, 0, "live", 114.9), now)
	e := clockEvent("opening", 3, "live", 114.6)
	e.LiveDuration = 115
	e.Time = now + 300
	if err := a.checkRoundJobLocked(Job{Events: []Event{e}}); err != nil {
		t.Fatal("opening recording failed during normal phase boundary:", err)
	}
}

func TestCompactTimingSynchronizesBothSides(t *testing.T) {
	b := testService(t, "agent")
	server := httptest.NewServer(b.Handler())
	defer server.Close()
	a := testService(t, "director")
	a.s.Config.WorkerURL = server.URL
	a.s.Config.Transition = 2
	b.s.Config.Transition = 2
	w := request(t, a, "POST", "/api/timing/compact", map[string]any{}, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, s := range []*Service{a, b} {
		if s.s.Config.Guard != .2 || s.s.Config.Setup != .2 || s.s.Config.Transition != 0 {
			t.Fatal(s.s.Config)
		}
	}
}
