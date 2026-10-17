package replay

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestShortMultiKillExportsOneArtifact(t *testing.T) {
	a := testService(t, "agent")
	c := a.s.Config
	e1 := ev("kill-one", "A", nowMS()-5000)
	e2 := e1
	e2.ID = "kill-two"
	e2.Time += 800
	j := Job{ID: "multi-kill", Player: "A", Epoch: c.Epoch, Map: c.Map, Match: c.Match, Start: e1.Time - 1200, End: e2.Time + 700, Demo: true, Events: []Event{e1, e2}}
	arts, err := a.capture(j, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || !arts[0].includesEvent(e1.ID) || !arts[0].includesEvent(e2.ID) {
		t.Fatal(arts)
	}
	start, _ := eventClipBounds(j, e1, c)
	_, end := eventClipBounds(j, e2, c)
	if end-start != 2300 {
		t.Fatal("merged clip lost pre-roll or second kill", start, end)
	}
}

func TestClipGroupsPreserveCameraCutsAndRounds(t *testing.T) {
	e1 := ev("one", "A", 10000)
	e2 := e1
	e2.ID = "two"
	e2.Time += 3000
	e3 := e2
	e3.ID = "three"
	e3.Player = "B"
	e3.Time += 1000
	e4 := e3
	e4.ID = "four"
	e4.Player = "A"
	e4.Time += 1000
	e5 := e4
	e5.ID = "five"
	e5.Round++
	e5.Time += 100
	e6 := e5
	e6.ID = "six"
	e6.Time += 3001
	groups := clipGroups(Job{Events: []Event{e1, e2, e3, e4, e5, e6}})
	if len(groups) != 5 || len(groups[0]) != 2 {
		t.Fatal(groups)
	}
}

func TestSharedClipDownloadsOnceAndQueuesOnce(t *testing.T) {
	a := testService(t, "director")
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gets.Add(1); w.Write(demoVideo) }))
	defer server.Close()
	c := a.s.Config
	c.WorkerURL = server.URL
	art := Artifact{ID: "shared", EventID: "one", EventIDs: []string{"one", "two"}, SHA256: fmt.Sprintf("%x", sha256.Sum256(demoVideo)), Size: int64(len(demoVideo)), Round: 1}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { copy := art; results <- a.download(c, &copy) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if gets.Load() != 1 {
		t.Fatal("duplicate download", gets.Load())
	}
	a.s.Jobs = []Job{{ID: "job-one", Match: c.Match, Map: c.Map, Epoch: c.Epoch}, {ID: "job-two", Match: c.Match, Map: c.Map, Epoch: c.Epoch}}
	art.JobID = "job-one"
	a.jobUpdate("job-one", "READY", "", []Artifact{art})
	art.JobID = "job-two"
	a.jobUpdate("job-two", "READY", "", []Artifact{art})
	if len(a.s.Artifacts) != 1 || len(a.s.Queue) != 1 {
		t.Fatal("duplicate shared clip in library", a.s.Queue)
	}
}

func TestRoundCounterArrivesBeforeOverPhase(t *testing.T) {
	a := testService(t, "director")
	a.s.Config.Mode = "live"
	frame := func(raw, kills int, phase, roundPhase string, remaining float64) map[string]any {
		p := clockGSI(raw+1, kills, phase, remaining)
		p["round"] = map[string]any{"phase": roundPhase}
		obj(obj(p, "allplayers"), "A")["state"] = map[string]any{"round_kills": float64(kills)}
		return p
	}
	a.ingestGSI("a", frame(4, 0, "live", "live", 20))
	epoch := a.s.Config.Epoch
	a.ingestGSI("a", frame(5, 1, "live", "live", 19.9))
	a.ingestGSI("a", frame(5, 1, "live", "live", 19.8))
	a.ingestGSI("a", frame(5, 1, "over", "over", 7))
	if len(a.s.Events) != 1 || a.s.Events[0].Round != 5 || a.s.Events[0].Clock.Round != 5 || a.s.Config.Epoch != epoch {
		t.Fatal(a.s.Events, a.s.Config.Epoch)
	}
	// Countdown advances first; an old round.phase=over must not hold freeze back.
	a.ingestGSI("a", frame(5, 0, "freezetime", "over", 15))
	if a.gsiRound["a"]+1 != 6 {
		t.Fatal("new freeze held in previous round")
	}
	a.ingestGSI("a", frame(5, 1, "live", "live", 114))
	if len(a.s.Events) != 2 || a.s.Events[1].Round != 6 || a.s.Config.Epoch != epoch {
		t.Fatal(a.s.Events)
	}
}
