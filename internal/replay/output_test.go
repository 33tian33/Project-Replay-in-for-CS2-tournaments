package replay

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func outputFixture(t *testing.T) *Service {
	s := testService(t, "director")
	c := s.s.Config
	s.s.Jobs = []Job{{ID: "job", Match: c.Match, Map: c.Map, Epoch: c.Epoch}, {ID: "old", Match: c.Match, Map: c.Map, Epoch: c.Epoch - 1}}
	for i, k := range []string{"a", "b", "c", "old"} {
		j := "job"
		if k == "old" {
			j = "old"
		}
		path := filepath.Join(s.dir, "media", k+".mp4")
		os.WriteFile(path, []byte("media-"+k), 0600)
		s.s.Artifacts = append(s.s.Artifacts, Artifact{ID: k, JobID: j, Round: i/2 + 1, Path: path})
	}
	s.s.Queue = []string{"b", "c", "a", "old"}
	s.s.HalfQueue = []string{"c", "a"}
	s.s.Output.Transition1 = "intro.webm"
	s.s.Output.Transition2 = "outro.mp4"
	s.outputSeen = nowMS()
	s.outputClient = "obs"
	return s
}
func TestOutputRoundIsolationAndHalfOrdering(t *testing.T) {
	s := outputFixture(t)
	if e := s.startOutputLocked("round", 1); e != nil {
		t.Fatal(e)
	}
	want := []OutputItem{{"intro.webm", "transition"}, {"b", "replay"}, {"a", "replay"}, {"outro.mp4", "transition"}}
	if !reflect.DeepEqual(s.output.Items, want) {
		t.Fatal(s.output)
	}
	if e := s.startOutputLocked("half", 0); e == nil {
		t.Fatal("interrupted active output")
	}
	s.output = OutputSession{}
	if e := s.startOutputLocked("half", 0); e != nil {
		t.Fatal(e)
	}
	if s.output.Items[1].ID != "c" || s.output.Items[2].ID != "a" {
		t.Fatal(s.output)
	}
}
func TestOutputAcknowledgementAndExpiry(t *testing.T) {
	s := outputFixture(t)
	s.startOutputLocked("round", 1)
	path := "/output-api/ack"
	sid := s.output.ID
	ack := func(i int, client string) int {
		return request(t, s, "POST", path, map[string]any{"id": sid, "index": i, "client": client}, "", "").Code
	}
	if ack(0, "wrong") != 409 || ack(0, "obs") != 200 || ack(0, "obs") != 409 {
		t.Fatal("ack validation")
	}
	if w := request(t, s, "GET", "/output-api/state?client=second", nil, "", ""); w.Code != 409 {
		t.Fatal("multiple outputs")
	}
	if w := request(t, s, "GET", "/output-api/media/old", nil, "", ""); w.Code != 404 {
		t.Fatal("old media disclosed")
	}
	if w := request(t, s, "GET", "/output-api/media/a", nil, "", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request(t, s, "GET", "/api/state", nil, "", ""); w.Code != 200 {
		t.Fatal("direct access failed")
	}
	for i := 1; i < 4; i++ {
		if ack(i, "obs") != 200 {
			t.Fatal(i)
		}
	}
	if s.output.ID != "" {
		t.Fatal("did not return transparent")
	}
	s.startOutputLocked("round", 1)
	s.outputSeen = nowMS() - 4000
	s.tickOutputLocked()
	if s.output.ID != "" {
		t.Fatal("disconnect did not clear")
	}
}
func gsiFrame(round int, phase string) map[string]any {
	return map[string]any{"provider": map[string]any{"appid": 730.0}, "map": map[string]any{"name": "de_mirage", "round": float64(round), "phase": "live"}, "round": map[string]any{"phase": phase}}
}
func TestOutputGSIHalfPriorityAndDedup(t *testing.T) {
	s := outputFixture(t)
	for _, p := range []map[string]any{gsiFrame(11, "live"), gsiFrame(11, "over")} {
		if e := s.ingestGSI("a", p); e != nil {
			t.Fatal(e)
		}
	}
	if s.output.Kind != "half" {
		t.Fatal(s.output)
	}
	sid := s.output.ID
	s.ingestGSI("a", gsiFrame(11, "over"))
	if s.output.ID != sid {
		t.Fatal("duplicate trigger")
	}
	s.output = OutputSession{}
	s.ingestGSI("a", gsiFrame(11, "over"))
	if s.output.ID != "" {
		t.Fatal("retrigger after finish")
	}
	s.ingestGSI("a", gsiFrame(12, "live"))
	s.ingestGSI("a", gsiFrame(12, "over"))
	if s.output.Round != 13 || s.output.Kind != "round" {
		t.Fatal(s.output)
	}
	s.ingestGSI("a", gsiFrame(13, "live"))
	if s.output.ID == "" {
		t.Fatal("must retain replay at start of next round")
	}
	p := gsiFrame(13, "live")
	p["phase_countdowns"] = map[string]any{"phase": "live", "phase_ends_in": "107"}
	s.ingestGSI("a", p)
	if s.output.ID != "" {
		t.Fatal("1:47 must cut replay")
	}
}
func TestOutputColdSnapshotAndBRoundIgnored(t *testing.T) {
	s := outputFixture(t)
	p := gsiFrame(11, "over")
	p["phase_countdowns"] = map[string]any{"phase": "halftime"}
	s.ingestGSI("a", p)
	s.ingestGSI("a", p)
	if s.output.ID != "" {
		t.Fatal("stale snapshot triggered")
	}
	s.ingestGSI("b", gsiFrame(1, "live"))
	s.ingestGSI("b", gsiFrame(1, "over"))
	if s.output.ID != "" {
		t.Fatal("B triggered output")
	}
}
func TestOutputQueuePersistenceAndSettings(t *testing.T) {
	s := outputFixture(t)
	w := request(t, s, "POST", "/api/output/queue", map[string]any{"kind": "half", "ids": []string{"b", "a"}}, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, ids := range [][]string{{"a", "a"}, {"old"}, {"unknown"}} {
		if request(t, s, "POST", "/api/output/queue", map[string]any{"kind": "half", "ids": ids}, "", "").Code != 400 {
			t.Fatal(ids)
		}
	}
	c := s.s.Output
	c.Hotkey = "Ctrl+Shift+F8"
	c.HalfRound = 15
	if w = request(t, s, "POST", "/api/output/settings", c, "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	epoch := s.s.Config.Epoch
	s.Close()
	reopened, e := New(s.dir, "director")
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if !reflect.DeepEqual(reopened.s.HalfQueue, []string{"b", "a"}) || reopened.s.Output.HalfRound != 15 || reopened.s.Config.Epoch != epoch || reopened.output.ID != "" {
		t.Fatal("persistence")
	}
}
func TestOutputUploadAndErrorRecovery(t *testing.T) {
	s := outputFixture(t)
	r := httptest.NewRequest("POST", "/api/output/transition/1?name=effect.webm", bytes.NewBufferString("test-media"))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if !strings.HasSuffix(s.s.Output.Transition1, ".webm") {
		t.Fatal("upload")
	}
	s.startOutputLocked("round", 1)
	w = request(t, s, "POST", "/output-api/ack", map[string]any{"id": s.output.ID, "index": 0, "client": "obs", "error": "decode"}, "", "")
	if w.Code != 200 || s.output.ID != "" || s.outputError == "" {
		t.Fatal("decode error recovery")
	}
}
func TestOutputPendingWaitsForRoundJob(t *testing.T) {
	s := outputFixture(t)
	s.s.Jobs[0].Status = "TRANSFERRING"
	s.s.Jobs[0].Events = []Event{{Round: 1}}
	s.scheduleOutputLocked("round", 1)
	s.output.Due = nowMS() - 1
	s.tickOutputLocked()
	if s.output.Due != 0 || len(s.output.Items) != 3 {
		t.Fatal("ready media must play before pending transfer", s.output)
	}
	s.s.Jobs[0].Status = "READY"
	s.tickOutputLocked()
	if s.output.Due != 0 || len(s.output.Items) != 4 {
		t.Fatal(s.output)
	}
}
func TestHotkeyParser(t *testing.T) {
	for _, v := range []string{"Ctrl+Alt+R", "Shift+F8", "Ctrl+1", ""} {
		if _, _, e := parseHotkey(v); e != nil {
			t.Fatal(v, e)
		}
	}
	for _, v := range []string{"R", "Win+R", "Ctrl+F12", "Ctrl+", "Ctrl+F99"} {
		if _, _, e := parseHotkey(v); e == nil {
			t.Fatal(v)
		}
	}
}
func TestOutputStateIsJSON(t *testing.T) {
	s := outputFixture(t)
	w := request(t, s, "GET", "/api/state", nil, "", "")
	var v map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil || v["output_url"] == nil {
		t.Fatal(e)
	}
}
