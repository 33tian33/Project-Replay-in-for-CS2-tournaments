package replay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHighlightsAutoSelectAllRecordedKillsAndDeduplicate(t *testing.T) {
	s := testService(t, "director")
	c := s.s.Config
	s.s.Jobs = []Job{{ID: "new", Match: c.Match, Map: c.Map, Epoch: c.Epoch}, {ID: "old", Match: c.Match, Map: c.Map, Epoch: c.Epoch - 1}}
	clips := []Artifact{{ID: "first", JobID: "new", Round: 1}, {ID: "last-half", JobID: "new", Round: 12}, {ID: "second", JobID: "new", Round: 13}, {ID: "overtime", JobID: "new", Round: 27}}
	s.jobUpdate("new", "READY", "", clips)
	s.jobUpdate("new", "READY", "", clips)
	s.jobUpdate("old", "READY", "", []Artifact{{ID: "stale", JobID: "old", Round: 1}})
	if !reflect.DeepEqual(s.s.HalfQueue, []string{"first", "last-half"}) || !reflect.DeepEqual(s.s.FullQueue, []string{"first", "last-half", "second", "overtime"}) {
		t.Fatal(s.s.HalfQueue, s.s.FullQueue)
	}
	s.s.Queue = nil
	s.outputSeen = nowMS()
	if err := s.startOutputLocked("full", 0); err != nil {
		t.Fatal(err)
	}
	if len(s.output.Items) != 4 {
		t.Fatal(s.output)
	}
}

func TestHighlightsWaitForLateTransferAndComplete(t *testing.T) {
	for _, kind := range []string{"half", "full"} {
		t.Run(kind, func(t *testing.T) {
			s := testService(t, "director")
			c := s.s.Config
			e := Event{ID: "last-kill", Match: c.Match, Map: c.Map, Epoch: c.Epoch, Round: 12, Status: "DOWNLOADING", JobID: "late"}
			s.s.Events = []Event{e}
			s.s.Jobs = []Job{{ID: "late", Match: c.Match, Map: c.Map, Epoch: c.Epoch, Status: "DOWNLOADING", Events: []Event{e}}}
			s.s.Output.Transition2 = "outro.webm"
			s.outputSeen = nowMS()
			s.outputClient = "obs"
			if err := s.startOutputLocked(kind, 12); err != nil {
				t.Fatal(err)
			}
			s.tickOutputLocked()
			if s.output.ID == "" || len(s.output.Items) != 0 {
				t.Fatal("must wait for pending kill", s.output)
			}
			s.jobUpdate("late", "READY", "", []Artifact{{ID: "late-clip", JobID: "late", Round: 12}})
			s.tickOutputLocked()
			if !reflect.DeepEqual(s.output.Items, []OutputItem{{"late-clip", "replay"}, {"outro.webm", "transition"}}) {
				t.Fatal(s.output)
			}
			for i := 0; i < 2; i++ {
				if w := request(t, s, "POST", "/output-api/ack", map[string]any{"id": s.output.ID, "index": i, "client": "obs"}, "", ""); w.Code != 200 {
					t.Fatal(w.Body.String())
				}
			}
			if s.output.ID != "" {
				t.Fatal("did not finish", s.output)
			}
		})
	}
}

func TestFullHighlightsGameoverPriorityDedupAndColdStart(t *testing.T) {
	s := outputFixture(t)
	// Use the detector directly so fixture map changes do not invalidate its clips.
	s.previous["a"] = gsiFrame(25, "live")
	s.output = OutputSession{ID: "round", Kind: "round", Round: 26}
	p := gsiFrame(26, "over")
	obj(p, "map")["phase"] = "gameover"
	s.detectOutputLocked(p)
	if s.output.Kind != "full" || s.output.Due == 0 {
		t.Fatal(s.output)
	}
	sid := s.output.ID
	s.previous["a"] = p
	s.detectOutputLocked(p)
	if s.output.ID != sid {
		t.Fatal("duplicate full highlights")
	}
	s.output = OutputSession{}
	s.detectOutputLocked(p)
	if s.output.ID != "" {
		t.Fatal("retriggered completed highlights")
	}
	s.fullTriggered = false
	s.previous["a"] = nil
	s.detectOutputLocked(p)
	if s.output.ID != "" {
		t.Fatal("cold gameover triggered")
	}
	s.previous["a"] = gsiFrame(25, "live")
	s.s.Output.AutoFull = false
	s.detectOutputLocked(p)
	if s.output.ID != "" {
		t.Fatal("disabled automatic full highlights")
	}
}

func TestHalfHighlightsSurviveNextRound(t *testing.T) {
	s := outputFixture(t)
	s.previous["a"] = gsiFrame(11, "over")
	s.output = OutputSession{ID: "half", Kind: "half", Round: 12}
	p := gsiFrame(12, "live")
	p["phase_countdowns"] = map[string]any{"phase": "live", "phase_ends_in": "106"}
	s.detectOutputLocked(p)
	if s.output.ID != "half" {
		t.Fatal("next round cancelled half highlights")
	}
}

func TestHighlightsMigrationAndPersistence(t *testing.T) {
	s := outputFixture(t)
	s.s.HighlightsVersion = 0
	s.s.FullQueue = nil
	s.saveLocked()
	dir := s.dir
	s.Close()
	// Simulate the old persisted configuration, including a retired relay token.
	path := filepath.Join(dir, "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	json.Unmarshal(raw, &disk)
	state := disk["state"].(map[string]any)
	state["config"].(map[string]any)["relay_token"] = "retired-secret"
	delete(state["output"].(map[string]any), "auto_full")
	raw, _ = json.Marshal(disk)
	os.WriteFile(path, raw, 0600)
	s, err = New(dir, "director")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.s.HalfQueue) != 3 || len(s.s.FullQueue) != 3 || !s.s.Output.AutoFull {
		t.Fatal(s.s)
	}
	s.s.FullQueue = []string{"b", "a"}
	s.s.Output.AutoFull = false
	s.saveLocked()
	s.Close()
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "relay_token") {
		t.Fatal("obsolete token persisted")
	}
	s, err = New(dir, "director")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !reflect.DeepEqual(s.s.FullQueue, []string{"b", "a"}) || s.s.Output.AutoFull {
		t.Fatal("edits lost on restart")
	}
}

func TestRecordingDefaultsKeepXRayEnabled(t *testing.T) {
	s := testService(t, "agent")
	for _, custom := range []bool{false, true} {
		s.s.TeamHUD = custom
		if !strings.Contains(s.recordingHUDCommand(), "spec_show_xray 1") {
			t.Fatal("X-ray not enabled")
		}
	}
}
