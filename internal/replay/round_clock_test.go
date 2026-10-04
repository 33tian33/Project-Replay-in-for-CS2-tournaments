package replay

import (
	"bufio"
	"fmt"
	"github.com/gorilla/websocket"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func clockGSI(round, kills int, phase string, remaining float64) map[string]any {
	p := gsiStats(round-1, kills)
	p["phase_countdowns"] = map[string]any{"phase": phase, "phase_ends_in": fmt.Sprintf("%.3f", remaining)}
	p["player"] = map[string]any{"steamid": "A"}
	return p
}
func clockEvent(id string, round int, phase string, remaining float64) Event {
	e := ev(id, "A", 0)
	e.Round = round
	e.RoundTiming = true
	e.Clock = &RoundClock{round, phase, remaining}
	e.Quality = "inferred"
	return e
}
func TestRoundClockResolution(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	now := nowMS()
	a.s.Events = []Event{clockEvent("event", 5, "live", 75)}
	a.roundClocks = map[string]clockSample{"b": {RoundClock{5, "live", 80}, now, now}}
	a.resolveRoundEventsLocked(now)
	if a.s.Events[0].Time != now+5000 {
		t.Fatal(a.s.Events)
	}
	// No manual calibration or command-slot mapping is needed to plan.
	if got := Plan(a.s.Events, nil, a.s.Config, now); got[0].Status != "SCHEDULED" {
		t.Fatal(got)
	}
	for _, tt := range []struct {
		round    int
		phase    string
		progress int64
		want     string
	}{
		{4, "live", now, "WAITING_CLOCK"}, {5, "bomb", now, "MISSED_WINDOW"}, {6, "live", now, "MISSED_WINDOW"}, {5, "live", 0, "WAITING_CLOCK"},
	} {
		a.roundClocks["b"] = clockSample{RoundClock{tt.round, tt.phase, 80}, now, tt.progress}
		a.resolveRoundEventsLocked(now)
		if a.s.Events[0].Time != 0 || a.s.Events[0].Status != tt.want {
			t.Fatal(tt, a.s.Events)
		}
	}
}
func TestRoundClockMissingMalformedAndPhaseTransition(t *testing.T) {
	if readRoundClock(gsiStats(0, 0)) != nil {
		t.Fatal("invented countdown")
	}
	for _, v := range []any{"NaN", "-1", "Infinity", nil} {
		p := clockGSI(1, 0, "live", 80)
		obj(p, "phase_countdowns")["phase_ends_in"] = v
		if readRoundClock(p) != nil {
			t.Fatal(v)
		}
	}
	a := testService(t, "director")
	a.s.Config.Mode = "live"
	a.ingestGSI("a", clockGSI(3, 0, "live", 50))
	a.ingestGSI("a", clockGSI(3, 1, "over", 7))
	if len(a.s.Events) != 1 || a.s.Events[0].Clock.Phase != "live" || a.s.Events[0].Tick != nil || a.s.Events[0].Time != 0 {
		t.Fatal(a.s.Events)
	}
}
func TestRoundEventIdempotencyAndSafePlayerNames(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	e := clockEvent("round-event", 1, "live", 85)
	e.Epoch = 99
	if _, err := a.acceptRoundEvent(e); err != nil {
		t.Fatal(err)
	}
	if _, err := a.acceptRoundEvent(e); err != nil || len(a.s.Events) != 1 {
		t.Fatal(err)
	}
	changed := jsonCopy(e)
	changed.Clock.Remaining = 84
	if _, err := a.acceptRoundEvent(changed); err == nil {
		t.Fatal("changed event accepted")
	}
	a.previous["b"] = clockGSI(1, 0, "live", 90)
	cmd, err := a.playerCommandLocked(Job{Player: "A"}, a.s.Config)
	if err != nil || cmd != `spec_player "Player"` {
		t.Fatal(cmd, err)
	}
	for _, name := range []string{"x;quit", "x\nquit", `x"`, "14"} {
		obj(obj(a.previous["b"], "allplayers"), "A")["name"] = name
		if _, err := a.playerCommandLocked(Job{Player: "A"}, a.s.Config); err == nil {
			t.Fatal(name)
		}
	}
}
func TestRoundDefaultsAndManualPause(t *testing.T) {
	a, err := New(t.TempDir(), "director")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.s.Config.Mode != "live" || a.s.Config.Paused || !a.s.Config.AutoCapture {
		t.Fatal(a.s.Config)
	}
	request(t, a, "POST", "/api/pause", map[string]bool{"paused": true}, "", "")
	a.Close()
	b, err := New(a.dir, "director")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !b.s.Config.Paused || b.s.Config.AutoCapture {
		t.Fatal("manual pause lost")
	}
}
func TestRoundClockLivePipelineWithoutCalibrationTicksOrMappings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell encoder fixture")
	}
	worker := testService(t, "agent")
	worker.s.Config.Mode = "live"
	worker.s.Config.Strict = true
	worker.s.Config.SessionLock = filepath.Join(t.TempDir(), "capture.lock")
	// Synthetic encoder output; real job execution and all HTTP/OBS handshakes remain exercised.
	dir := t.TempDir()
	sample := filepath.Join(dir, "sample.mp4")
	os.WriteFile(sample, demoVideo, 0600)
	encoder := filepath.Join(dir, "encoder")
	probe := filepath.Join(dir, "probe")
	os.WriteFile(encoder, []byte("#!/bin/sh\nfor last; do :; done\ncp '"+sample+"' \"$last\"\n"), 0700)
	os.WriteFile(probe, []byte("#!/bin/sh\necho '{\"format\":{\"duration\":\"1.5\"},\"streams\":[{\"codec_type\":\"video\",\"codec_name\":\"h264\"}]}'\n"), 0700)
	worker.s.Config.FFmpeg = encoder
	worker.s.Config.FFprobe = probe
	var switched atomic.Bool
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	worker.s.Config.NetCon = listener.Addr().String()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			c.Close()
			if strings.HasPrefix(line, `spec_player "Player"`) {
				switched.Store(true)
			}
		}
	}()
	up := websocket.Upgrader{}
	obsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteJSON(map[string]any{"op": 0, "d": map[string]any{}})
		var hello map[string]any
		if c.ReadJSON(&hello) != nil {
			return
		}
		c.WriteJSON(map[string]any{"op": 2, "d": map[string]any{}})
		active := false
		var start int64
		for {
			var request map[string]any
			if c.ReadJSON(&request) != nil {
				return
			}
			d := obj(request, "d")
			result := map[string]any{}
			switch stringField(d, "requestType") {
			case "GetRecordStatus":
				result["outputActive"] = active
				result["outputDuration"] = max(nowMS()-start, 0)
			case "StartRecord":
				active = true
				start = nowMS()
			case "StopRecord":
				active = false
				result["outputPath"] = sample
			}
			c.WriteJSON(map[string]any{"op": 7, "d": map[string]any{"requestId": d["requestId"], "requestStatus": map[string]any{"result": true, "code": 100}, "responseData": result}})
		}
	}))
	defer obsServer.Close()
	worker.s.Config.OBSURL = "ws" + strings.TrimPrefix(obsServer.URL, "http")
	server := httptest.NewServer(worker.Handler())
	defer server.Close()
	director := testService(t, "director")
	director.s.Config.Mode = "live"
	director.s.Config.WorkerURL = server.URL
	director.s.Config.Strict = true
	// Both services have no calibration and no mappings; worker has an unrelated epoch.
	worker.s.Config.Epoch = 17
	start := nowMS()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				p := clockGSI(1, 0, "live", 100-float64(nowMS()-start)/1000)
				if !switched.Load() {
					p["player"] = map[string]any{"steamid": "other"}
				}
				worker.ingestGSI("b", p)
			}
		}
	}()
	defer func() { close(stop); <-done }()
	director.ingestGSI("a", clockGSI(1, 0, "live", 97.1))
	director.ingestGSI("a", clockGSI(1, 1, "live", 97))
	worker.Start()
	director.Start()
	defer director.Close()
	defer worker.Close()
	for deadline := nowMS() + 8000; nowMS() < deadline; {
		director.mu.Lock()
		ready := len(director.s.Artifacts) == 1
		director.mu.Unlock()
		if ready {
			if !switched.Load() {
				t.Fatal("no player command")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	director.mu.Lock()
	defer director.mu.Unlock()
	worker.mu.Lock()
	defer worker.mu.Unlock()
	t.Fatalf("pipeline incomplete: director=%+v worker events=%+v jobs=%+v", director.s.Jobs, worker.s.Events, worker.s.Jobs)
}

func TestPausedCountdownInvalidatesCommittedWindow(t *testing.T) {
	a := testService(t, "agent")
	now := nowMS()
	e := clockEvent("kill", 1, "live", 50)
	e.Time = now + 2000
	j := Job{Events: []Event{e}}
	a.roundClocks = map[string]clockSample{"b": {RoundClock{1, "live", 52}, now, now}}
	if err := a.checkRoundJobLocked(j); err != nil {
		t.Fatal(err)
	}
	a.roundClocks["b"] = clockSample{RoundClock{1, "live", 54}, now, now}
	if err := a.checkRoundJobLocked(j); err == nil {
		t.Fatal("timeline jump accepted")
	}
	a.roundClocks["b"] = clockSample{RoundClock{1, "live", 52}, now, now - 2000}
	if err := a.checkRoundJobLocked(j); err == nil {
		t.Fatal("paused clock accepted")
	}
}
