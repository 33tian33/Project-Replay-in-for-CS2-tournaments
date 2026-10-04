package replay

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func gsiStats(round, kills int) map[string]any {
	var p map[string]any
	json.Unmarshal([]byte(`{"provider":{"appid":730},"map":{"name":"de_mirage","round":1},"allplayers":{"A":{"name":"Player","match_stats":{"kills":0}}}}`), &p)
	obj(p, "map")["round"] = float64(round)
	obj(obj(obj(p, "allplayers"), "A"), "match_stats")["kills"] = float64(kills)
	return p
}
func TestGSIMatchStatsFallbackAndRoundBoundary(t *testing.T) {
	a := testService(t, "director")
	for _, p := range []map[string]any{gsiStats(1, 10), gsiStats(1, 11), gsiStats(1, 11), gsiStats(2, 12), gsiStats(2, 13)} {
		if err := a.ingestGSI("a", p); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.s.Events) != 3 || a.s.Events[0].Kills != 1 || a.s.Events[1].Kills != 1 || a.s.Events[2].Kills != 2 {
		t.Fatal(a.s.Events)
	}
	if a.gsiDiagnostics["a"].Detected != 3 || a.gsiDiagnostics["a"].Counters != 1 {
		t.Fatal(a.gsiDiagnostics)
	}
}
func TestGSIMissingCounterDoesNotAssumeZero(t *testing.T) {
	a := testService(t, "director")
	p := gsiStats(1, 4)
	delete(obj(obj(p, "allplayers"), "A"), "match_stats")
	a.ingestGSI("a", p)
	a.ingestGSI("a", gsiStats(1, 7))
	if len(a.s.Events) != 0 {
		t.Fatal("invented kills from missing counter")
	}
	a.ingestGSI("a", gsiStats(1, 9))
	if len(a.s.Events) != 1 || a.s.Events[0].GroupCount != 2 {
		t.Fatal(a.s.Events)
	}
}
func TestGSIAcceptsWithoutPairing(t *testing.T) {
	a := testService(t, "director")
	p := gsiStats(1, 0)
	w := request(t, a, "POST", "/gsi/a", p, "", "")
	if w.Code != 200 || a.gsiDiagnostics["a"].Error != "" {
		t.Fatal(w.Code, a.gsiDiagnostics)
	}
	p["auth"] = map[string]any{"token": "obsolete-value-ignored"}
	w = request(t, a, "POST", "/gsi/a", p, "", "")
	if w.Code != 200 || a.gsiDiagnostics["a"].Error != "" {
		t.Fatal(w.Code, a.gsiDiagnostics)
	}
}
func TestGSITriggersRemoteRecordingAutomatically(t *testing.T) {
	worker := testService(t, "agent")
	worker.s.Config.Setup = .2
	worker.s.Config.Epoch = 17
	worker.s.Config.Paused = false
	worker.s.Config.CalibratedUntil = nowMS() + 60000
	server := httptest.NewServer(worker.Handler())
	defer server.Close()
	director := testService(t, "director")
	director.s.Config.WorkerURL = server.URL
	director.s.Config.Setup = .2
	director.s.Config.Guard = .25
	director.s.Config.Delta = 2.5
	director.s.Config.Paused = false
	for _, kills := range []int{20, 21} {
		p := gsiStats(1, kills)
		if w := request(t, director, "POST", "/gsi/a", p, "", ""); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	director.Start()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		director.mu.Lock()
		ready := len(director.s.Artifacts) == 1
		director.mu.Unlock()
		if ready {
			worker.mu.Lock()
			defer worker.mu.Unlock()
			if len(worker.s.Jobs) != 1 || worker.s.Jobs[0].Status != "READY" {
				t.Fatal(worker.s.Jobs)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	director.mu.Lock()
	defer director.mu.Unlock()
	t.Fatalf("GSI did not finish remote recording: events=%+v jobs=%+v", director.s.Events, director.s.Jobs)
}
