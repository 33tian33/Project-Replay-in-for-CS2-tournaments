package replay

import (
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestManualReplaySuppressesRoundEndAndHotkey(t *testing.T) {
	for _, hotkey := range []bool{false, true} {
		t.Run(map[bool]string{false: "button", true: "hotkey"}[hotkey], func(t *testing.T) {
			s := outputFixture(t)
			s.ingestGSI("a", gsiFrame(0, "live"))
			if hotkey {
				s.triggerHotkey()
			} else if w := request(t, s, "POST", "/api/output/play", map[string]any{"kind": "round", "round": 1}, "", ""); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			request(t, s, "POST", "/api/output/stop", map[string]any{}, "", "")
			s.ingestGSI("a", gsiFrame(0, "over"))
			if s.output.ID != "" {
				t.Fatal("manual replay was replayed automatically")
			}
		})
	}
}

func TestReplayLateMediaAppendsAndFirstKillCuts(t *testing.T) {
	s := outputFixture(t)
	s.s.Jobs[0].Status = "TRANSFERRING"
	s.s.Jobs[0].Events = []Event{{Round: 1}}
	if err := s.startOutputLocked("round", 1); err != nil {
		t.Fatal(err)
	}
	sid := s.output.ID
	for i := 0; i < 3; i++ {
		if w := request(t, s, "POST", "/output-api/ack", map[string]any{"id": sid, "index": i, "client": "obs"}, "", ""); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if s.output.ID != sid || s.output.Index != 3 {
		t.Fatal("pending media lost session", s.output)
	}
	s.jobUpdate("job", "READY", "", []Artifact{{ID: "late", JobID: "job", Round: 1}})
	s.tickOutputLocked()
	if s.output.Items[3].ID != "late" || s.output.Items[4].ID != "outro.mp4" {
		t.Fatal("late item must precede closing transition", s.output)
	}
	s.ingestGSI("a", gsiFrame(0, "over"))
	p := gsiFrame(1, "live")
	p["phase_countdowns"] = map[string]any{"phase": "live", "phase_ends_in": "114"}
	s.ingestGSI("a", p)
	if s.output.ID == "" {
		t.Fatal("cut before deadline")
	}
	p = gsiFrame(1, "live")
	p["allplayers"] = map[string]any{"killer": map[string]any{"state": map[string]any{"round_kills": 1.0}}}
	s.ingestGSI("b", p)
	if s.output.ID == "" {
		t.Fatal("B must not cut output")
	}
	s.ingestGSI("a", p)
	if s.output.ID != "" {
		t.Fatal("A first kill must cut output")
	}
	s.tickOutputLocked()
	if s.output.ID != "" {
		t.Fatal("late queue must not restart cut session")
	}
}

func TestPriorityPreservesClutchAndTwoSecondChain(t *testing.T) {
	c := testConfig()
	a, b, trade := ev("a", "A", 10000), ev("b", "A", 12000), ev("trade", "B", 12100)
	if got := chosen(Plan([]Event{trade, b, a}, nil, c, 0)); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatal(got)
	}
	for _, enemies := range []int{2, 3} {
		clutch := ev("clutch", "C", 11500)
		clutch.Clutch = enemies
		if got := chosen(Plan([]Event{ev("before", "X", 11000), ev("after", "Y", 12000), clutch}, nil, c, 0)); !reflect.DeepEqual(got, []string{"clutch"}) {
			t.Fatal(got)
		}
	}
	players := map[string]any{}
	for name, team := range map[string]string{"C": "CT", "x": "T", "y": "T"} {
		players[name] = map[string]any{"team": team, "state": map[string]any{"health": 100.0}}
	}
	if clutchOpponents(players, "C") != 2 || clutchOpponents(players, "x") != 0 {
		t.Fatal("incorrect clutch detection")
	}
}

func TestConfigSendsGOTVOnlyWhenEnabled(t *testing.T) {
	worker := demoService(t, "agent")
	server := httptest.NewServer(worker.Handler())
	defer server.Close()
	director := testService(t, "director")
	c := director.s.Config
	c.Mode = "live"
	c.WorkerURL = server.URL
	c.GOTV = "127.0.0.1:27020"
	save := func() {
		t.Helper()
		w := request(t, director, "POST", "/api/config", c, "", "")
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	save()
	if worker.s.Config.GOTV != "" {
		t.Fatal("opt-out forwarded GOTV")
	}
	c.AutoSendGOTV = true
	save()
	if worker.s.Config.GOTV != c.GOTV || worker.s.Config.Paused {
		t.Fatal("GOTV not saved or capture paused")
	}
	c.GOTV = "host:27020;quit"
	w := request(t, director, "POST", "/api/config", c, "", "")
	if w.Code != 400 || worker.s.Config.GOTV == c.GOTV {
		t.Fatal("invalid command accepted")
	}
}

func TestBatchedGSIKillsKeepSingleCameraCandidate(t *testing.T) {
	c := testConfig()
	multi := ev("multi", "A", 10000)
	multi.RoundTiming = true
	multi.GroupCount = 2
	rival := ev("rival", "B", 10010)
	if got := chosen(Plan([]Event{rival, multi}, nil, c, 0)); !reflect.DeepEqual(got, []string{"multi"}) {
		t.Fatal(got)
	}
}
