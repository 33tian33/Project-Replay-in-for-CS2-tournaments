package replay

import (
	"math"
	"testing"
)

func TestPostPlantDefuseClockAndKill(t *testing.T) {
	a := testService(t, "director")
	a.s.Config.Mode = "live"
	for _, frame := range []struct {
		phase     string
		remaining float64
		kills     int
	}{{"bomb", 30, 0}, {"defuse", 5, 0}, {"defuse", 4, 1}, {"bomb", 28, 1}, {"defuse", 5, 1}, {"defuse", 4, 2}} {
		if err := a.ingestGSI("a", clockGSI(5, frame.kills, frame.phase, frame.remaining)); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.s.Events) != 2 {
		t.Fatalf("lost defuse kills: %+v", a.s.Events)
	}
	for i, e := range a.s.Events {
		want := 29 - float64(i)*2
		if e.Clock == nil || e.Clock.Phase != "bomb" || math.Abs(e.Clock.Remaining-want) > .2 {
			t.Fatalf("defuse timer used as bomb fuse: %+v", e.Clock)
		}
		b := testService(t, "agent")
		b.s.Config.Mode = "live"
		b.s.Config.Match, b.s.Config.Map = e.Match, e.Map
		if _, err := b.acceptRoundEvent(e); err != nil {
			t.Fatal(err)
		}
		now := nowMS()
		b.roundClocks = map[string]clockSample{"b": {RoundClock{5, "bomb", e.Clock.Remaining + 10}, now, now}}
		b.resolveRoundEventsLocked(now)
		if math.Abs(float64(b.s.Events[0].Time-(now+10000))) > 1 {
			t.Fatalf("wrong B timing: %+v", b.s.Events[0])
		}
	}
}

func TestDefuseClockPauseAndMissingBaseline(t *testing.T) {
	a := testService(t, "director")
	p := clockGSI(5, 0, "defuse", 5)
	a.updateRoundClockForRoundLocked("a", p, 10000, 5)
	if _, ok := a.roundClocks["a"]; ok {
		t.Fatal("invented bomb clock on cold defuse")
	}
	a.roundClocks["a"] = clockSample{RoundClock{5, "bomb", 30}, 10000, 10000}
	a.previous["a"] = p
	a.updateRoundClockForRoundLocked("a", p, 11000, 5)
	if a.roundClocks["a"].Clock.Remaining != 30 || a.roundClocks["a"].ProgressAt != 10000 {
		t.Fatal("paused defuse advanced bomb fuse", a.roundClocks["a"])
	}
}
