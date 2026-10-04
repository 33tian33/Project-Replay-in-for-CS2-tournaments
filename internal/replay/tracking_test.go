package replay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func utilityEvent(id, player, instance string, ms int64) Event {
	e := ev(id, player, ms)
	e.Utility = &Utility{Kind: "hegrenade", ID: instance, Evidence: "fixture throw/death linkage"}
	return e
}
func TestUtilityPlanner(t *testing.T) {
	c := testConfig()
	gun := ev("gun", "A", 10000)
	a := utilityEvent("a", "A", "throw1", 10100)
	b := utilityEvent("b", "A", "throw1", 10200)
	other := utilityEvent("other", "A", "throw2", 10250)
	if got := chosen(Plan([]Event{gun, a, b, other}, nil, c, 0)); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatal(got)
	}
	gun.Pin, a.Pin = true, true
	if got := Plan([]Event{gun, a}, nil, c, 0); got[0].Status != "PIN_CONFLICT" {
		t.Fatal(got)
	}
	a.Pin = false
	a.Utility.ID = ""
	if got := Plan([]Event{a}, nil, c, 0); got[0].Status != "TRACKING_UNRESOLVED" {
		t.Fatal(got)
	}
	a.Utility.ID = "throw1"
	c.Mode = "live"
	c.Strict = false
	c.CalibratedUntil = 99999
	if got := Plan([]Event{a}, nil, c, 0); got[0].Status != "TRACKING_UNAVAILABLE" {
		t.Fatal(got)
	}
	c.TrackingURL = "http://127.0.0.1:1234"
	// Utility camera does not require a player slot.
	if got := chosen(Plan([]Event{a}, nil, c, 0)); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatal(got)
	}
}
func TestUtilityValidation(t *testing.T) {
	for _, u := range []*Utility{{Kind: "gun", ID: "a"}, {Kind: "hegrenade", ID: "../bad"}, {Kind: "molotov", ID: "a", Evidence: strings.Repeat("x", 4001)}} {
		if validateUtility(u) == nil {
			t.Fatal(u)
		}
	}
	for _, raw := range []string{"file:///tmp/camera", "http://user:pass@localhost", "http://localhost?token=x"} {
		if validateTrackingURL(raw) == nil {
			t.Fatal(raw)
		}
	}
}
func TestUtilityMonitorEvidence(t *testing.T) {
	s := &Service{ctx: context.Background(), s: State{Config: Config{Epoch: 1}}}
	e := utilityEvent("a", "A", "throw1", 10000)
	j := Job{ID: "job1", Epoch: 1, Utility: e.Utility}
	good := func() trackingSample {
		return trackingSample{JobID: j.ID, Epoch: 1, UtilityID: e.Utility.ID, Phase: "projectile", ObservedAt: nowMS(), CameraVerified: true, Evidence: "actual camera/entity sample"}
	}
	var change func(*trackingSample)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tracking/job1" || r.Header.Get("Authorization") != "" {
			t.Error("request contract")
		}
		sample := good()
		if change != nil {
			change(&sample)
		}
		json.NewEncoder(w).Encode(sample)
	}))
	defer server.Close()
	m := &utilityCamera{service: s, job: j, config: Config{TrackingURL: server.URL}}
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*trackingSample)
	}{
		{"wrong throw", func(s *trackingSample) { s.UtilityID = "other" }},
		{"wrong job", func(s *trackingSample) { s.JobID = "other" }},
		{"wrong epoch", func(s *trackingSample) { s.Epoch = 2 }},
		{"stale", func(s *trackingSample) { s.ObservedAt -= 1000 }},
		{"future", func(s *trackingSample) { s.ObservedAt += 1000 }},
		{"unverified", func(s *trackingSample) { s.CameraVerified = false }},
		{"no evidence", func(s *trackingSample) { s.Evidence = "" }},
		{"missing entity", func(s *trackingSample) { s.Phase = "lost" }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			change = tt.change
			if m.Check() == nil {
				t.Fatal("accepted invalid evidence")
			}
		})
	}
	change = func(s *trackingSample) { s.Phase = "effect" }
	for i := 0; i < 10; i++ {
		if err := m.Check(); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.Evidence()) != 4 {
		t.Fatal("unbounded phase evidence", m.Evidence())
	}
	change = nil
	if m.Check() == nil {
		t.Fatal("effect must not return to projectile")
	}
	s.s.Config.Epoch = 2
	if m.Check() == nil {
		t.Fatal("accepted stale session")
	}
}
func TestPrepareUtilityCameraLifecycle(t *testing.T) {
	var mu sync.Mutex
	methods := []string{}
	e := utilityEvent("e", "A", "throw", nowMS()+2000)
	j := Job{ID: "job", Epoch: 1, Utility: e.Utility, Start: nowMS() + 1500}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		if r.Method == "PUT" {
			var got Job
			if json.NewDecoder(r.Body).Decode(&got) != nil || got.Utility.ID != "throw" {
				t.Error("prepare did not carry target")
			}
		}
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(trackingSample{JobID: "job", Epoch: 1, UtilityID: "throw", Phase: "effect", ObservedAt: nowMS(), CameraVerified: true, Evidence: "fire area verified"})
		}
	}))
	defer server.Close()
	a := &Service{ctx: context.Background(), s: State{Config: Config{Epoch: 1}}}
	camera, err := a.prepareCamera(j, Config{TrackingURL: server.URL, NetCon: "invalid"})
	if err != nil {
		t.Fatal(err)
	}
	camera.Close()
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(methods, []string{"PUT", "GET", "DELETE"}) {
		t.Fatal(methods)
	}
}
func TestRejectUtilityBeforePlayerControl(t *testing.T) {
	a := &Service{ctx: context.Background()}
	j := Job{Utility: &Utility{Kind: "hegrenade", ID: "throw", Evidence: "link"}}
	_, err := a.prepareCamera(j, Config{NetCon: "invalid"})
	if err == nil || !strings.Contains(err.Error(), "TRACKING_UNAVAILABLE") {
		t.Fatal(err)
	}
}
func TestRemoteRejectsMismatchedUtility(t *testing.T) {
	a, err := New(t.TempDir(), "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := testConfig()
	c.CalibratedUntil = nowMS() + 60000
	a.s.Config = c
	e := utilityEvent("e", "A", "throw1", nowMS()+10000)
	start, end := bounds(e, c)
	j := Job{ID: "job", Epoch: c.Epoch, Match: c.Match, Map: c.Map, Player: e.Player, Utility: &Utility{Kind: "hegrenade", ID: "throw2", Evidence: "link"}, Events: []Event{e}, Start: start, End: end, Demo: true}
	if _, err = a.acceptJob(j); err == nil {
		t.Fatal("accepted different throw")
	}
	j.Utility = e.Utility
	j.Player = ""
	j.Events[0].Player = ""
	if _, err = a.acceptJob(j); err == nil {
		t.Fatal("accepted missing kill ownership")
	}
	j.Player = "A"
	j.Events[0].Player = "A"
	j.Utility = nil
	if _, err = a.acceptJob(j); err == nil {
		t.Fatal("accepted silent player fallback")
	}
}

func TestUtilityDemoArtifacts(t *testing.T) {
	a, err := New(t.TempDir(), "director")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	e := utilityEvent("e", "A", "throw", nowMS()-2000)
	a.s.Config.Map = e.Map // Synthetic fixture has no GSI feed.
	e2 := e
	e2.ID = "e2"
	e2.Time += 100
	j := Job{ID: "demo-job", Epoch: e.Epoch, Match: e.Match, Map: e.Map, Demo: true, Events: []Event{e, e2}, Start: e.Time - 1200, End: e2.Time + 1200, Utility: e.Utility}
	arts, err := a.capture(j, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 {
		t.Fatal(arts)
	}
	for _, art := range arts {
		if art.Duration != 1.5 || art.Utility.ID != "throw" || art.Player != "A" || art.CameraQuality != "demo" || len(art.CameraEvidence) != 0 {
			t.Fatal(art)
		}
	}
}
func TestOBSPasswordRedacted(t *testing.T) {
	a, err := New(t.TempDir(), "director")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.s.Config.OBSPassword = "private-obs-password"
	data, err := json.Marshal(a.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-obs-password") {
		t.Fatal("snapshot exposed OBS password")
	}
}
func TestTrackingReleaseFailurePauses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	a := &Service{ctx: context.Background()}
	m := &utilityCamera{service: a, job: Job{ID: "job"}, config: Config{TrackingURL: server.URL}}
	m.Close()
	m.Close()
	if !a.s.Config.Paused || len(a.s.Logs) != 1 {
		t.Fatal("release must pause once", a.s)
	}
}
