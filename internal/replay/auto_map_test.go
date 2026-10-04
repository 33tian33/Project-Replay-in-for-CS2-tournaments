package replay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGSIMapDiscoveryAndTransition(t *testing.T) {
	for _, role := range []string{"director", "agent"} {
		t.Run(role, func(t *testing.T) {
			s := testService(t, role)
			s.s.Config.Map = ""
			s.s.Config.Mode = "live"
			side := "a"
			other := "b"
			if role == "agent" {
				side, other = "b", "a"
			}
			epoch := s.s.Config.Epoch
			p := gsiFrame(0, "live")
			obj(p, "map")["name"] = "de_nuke"
			if err := s.ingestGSI(side, p); err != nil {
				t.Fatal(err)
			}
			if s.s.Config.Map != "de_nuke" || s.s.Config.Epoch != epoch+1 || s.s.Config.Paused {
				t.Fatal(s.s.Config)
			}
			s.ingestGSI(side, p)
			if s.s.Config.Epoch != epoch+1 {
				t.Fatal("same map changed epoch")
			}
			s.s.Queue = []string{"old"}
			s.s.HalfQueue = []string{"old"}
			s.s.FullQueue = []string{"old"}
			s.output = OutputSession{ID: "old"}
			s.manualRounds = map[int]bool{1: true}
			p = gsiFrame(0, "live")
			obj(p, "map")["name"] = "de_inferno"
			s.ingestGSI(other, p)
			if s.s.Config.Map != "de_nuke" || s.output.ID != "old" {
				t.Fatal("secondary feed changed map")
			}
			s.ingestGSI(side, p)
			if s.s.Config.Map != "de_inferno" || s.s.Config.Epoch != epoch+2 || len(s.s.Queue) != 0 || len(s.s.HalfQueue) != 0 || len(s.s.FullQueue) != 0 || s.output.ID != "" || len(s.manualRounds) != 0 {
				t.Fatal("map transition did not isolate output")
			}
			s.ingestGSI(side, map[string]any{"provider": map[string]any{"appid": 730.0}})
			if s.s.Config.Map != "de_inferno" || s.previous[side] != nil {
				t.Fatal("menu frame changed map or retained kill baseline")
			}
			s.Close()
			reopened, err := New(s.dir, role)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if reopened.s.Config.Map != "de_inferno" {
				t.Fatal("map not persisted")
			}
		})
	}
}

func TestMapCannotBeOverwrittenByStaleSettings(t *testing.T) {
	s := testService(t, "director")
	s.s.Config.Map = "de_nuke"
	c := s.s.Config
	c.Map = "de_dust2"
	if err := s.configure(c); err != nil {
		t.Fatal(err)
	}
	if s.s.Config.Map != "de_nuke" {
		t.Fatal("settings overwrote GSI map")
	}
	c = defaults()
	if c.Map != "" {
		t.Fatal("new installation must wait for GSI")
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNewMapKillUsesFreshBaseline(t *testing.T) {
	s := testService(t, "director")
	s.ingestGSI("a", gsiStats(1, 10))
	p := gsiStats(1, 20)
	obj(p, "map")["name"] = "de_nuke"
	s.ingestGSI("a", p)
	if len(s.s.Events) != 0 {
		t.Fatal("invented cross-map kills")
	}
	p = gsiStats(1, 21)
	obj(p, "map")["name"] = "de_nuke"
	s.ingestGSI("a", p)
	if len(s.s.Events) != 1 || s.s.Events[0].Map != "de_nuke" || s.s.Events[0].Epoch != s.s.Config.Epoch {
		t.Fatal(s.s.Events)
	}
}

func TestRemoteMapMismatchWaitsForBGSIDiscovery(t *testing.T) {
	worker := testService(t, "agent")
	worker.s.Config.Mode = "live"
	attempts := make(chan struct{}, 1)
	handler := worker.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		if r.Method == "POST" {
			select {
			case attempts <- struct{}{}:
			default:
			}
		}
	}))
	defer server.Close()
	director := testService(t, "director")
	director.s.Config.Map = "de_nuke"
	director.s.Config.WorkerURL = server.URL
	ctx, cancel := context.WithTimeout(director.ctx, 3*time.Second)
	defer cancel()
	director.ctx = ctx
	e := ev("newmap", "A", 0)
	e.Map = "de_nuke"
	e.Round = 1
	e.RoundTiming = true
	e.Clock = &RoundClock{Round: 1, Phase: "live", Remaining: 110}
	done := make(chan error, 1)
	go func() { var accepted Event; done <- director.submitRoundEvent(director.s.Config, e, &accepted) }()
	select {
	case <-attempts:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	worker.mu.Lock()
	unchanged := worker.s.Config.Map == "de_mirage"
	worker.mu.Unlock()
	if !unchanged {
		t.Fatal("event must not change worker map")
	}
	p := gsiFrame(0, "live")
	obj(p, "map")["name"] = "de_nuke"
	worker.ingestGSI("b", p)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if len(worker.s.Events) != 1 || worker.s.Events[0].Epoch != worker.s.Config.Epoch {
		t.Fatal(worker.s.Events)
	}
}

func TestMapDiscoveryPreservesTemporaryCapturePause(t *testing.T) {
	s := testService(t, "agent")
	s.s.Config.Map = ""
	s.localDemo.Phase = "armed"
	if err := s.ingestGSI("b", gsiFrame(0, "live")); err != nil {
		t.Fatal(err)
	}
	if !s.s.Config.Paused {
		t.Fatal("map discovery must not start an armed demo early")
	}
}
