package replay

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

func testConfig() Config {
	c := defaults()
	c.Map = "de_mirage"
	c.Mode = "demo"
	c.Paused = false
	c.Setup = .3
	c.Transition = .3
	c.Guard = .2
	return c
}
func ev(id, player string, ms int64) Event {
	return Event{ID: id, Player: player, Name: player, Time: ms, Match: "训练赛", Map: "de_mirage", Epoch: 1, Quality: "demo", GroupCount: 1, Uncertainty: .1}
}
func chosen(es []Event) []string {
	r := []string{}
	for _, e := range es {
		if e.Status == "SCHEDULED" {
			r = append(r, e.ID)
		}
	}
	sort.Strings(r)
	return r
}
func TestPlannerCases(t *testing.T) {
	c := testConfig()
	cases := []struct {
		name   string
		events []Event
		jobs   []Job
		want   []string
	}{
		{"double kill beats first arrival", []Event{ev("a", "A", 10000), ev("b", "B", 10200), ev("c", "B", 10700)}, nil, []string{"b", "c"}},
		{"nearby kills use direct cut", []Event{ev("a", "A", 10000), ev("b", "B", 12100)}, nil, []string{"a", "b"}},
		{"separate windows", []Event{ev("a", "A", 10000), ev("b", "B", 13000)}, nil, []string{"a", "b"}},
		{"committed reservation", []Event{ev("b", "B", 10200), ev("c", "B", 10700)}, []Job{{Start: 8800, End: 11200, Status: "CAPTURING"}}, []string{}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := chosen(Plan(tt.events, tt.jobs, c, 0))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("%v want %v", got, tt.want)
			}
		})
	}
}
func TestPinAndInvalidEvents(t *testing.T) {
	c := testConfig()
	a, b := ev("a", "A", 10000), ev("b", "B", 10100)
	b.Pin = true
	if got := chosen(Plan([]Event{a, b}, nil, c, 0)); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatal(got)
	}
	a.Pin = true
	r := Plan([]Event{a, b}, nil, c, 0)
	if r[0].Status != "PIN_CONFLICT" || len(chosen(r)) != 0 {
		t.Fatal(r)
	}
	cases := []struct {
		name   string
		change func(*Event, *Config)
		want   string
	}{
		{"epoch", func(e *Event, c *Config) { e.Epoch = 2 }, "CANCELLED"},
		{"batch", func(e *Event, c *Config) { e.GroupCount = 2 }, "UNCERTAIN"},
		{"uncertainty", func(e *Event, c *Config) { e.Uncertainty = .5 }, "UNCERTAIN"},
		{"late", func(e *Event, c *Config) { e.Time = 100 }, "MISSED_WINDOW"},
		{"strict", func(e *Event, c *Config) { c.Mode = "live"; c.Strict = true; c.Mappings[e.Player] = 14 }, "UNCERTAIN"},
		{"mapping", func(e *Event, c *Config) { c.Mode = "live" }, "IDENTITY_UNKNOWN"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			e := ev("a", "A", 10000)
			tt.change(&e, &c)
			if r := Plan([]Event{e}, nil, c, 0); r[0].Status != tt.want {
				t.Fatal(r)
			}
		})
	}
}
func TestPlannerAgainstExhaustiveSearch(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	c := testConfig()
	for trial := 0; trial < 100; trial++ {
		events := make([]Event, 8)
		for i := range events {
			events[i] = ev(string(rune('a'+i)), string(rune('A'+rng.Intn(3))), int64(5000+rng.Intn(12000)))
			events[i].Kills = rng.Intn(5) + 1
			events[i].Missed = rng.Intn(2) == 0
		}
		sort.Slice(events, func(i, j int) bool {
			if events[i].Time == events[j].Time {
				return events[i].Player < events[j].Player
			}
			return events[i].Time < events[j].Time
		})
		best := [5]int{}
		for mask := 1; mask < 1<<len(events); mask++ {
			score := [5]int{}
			prev := -1
			valid := true
			for i, e := range events {
				if mask&(1<<i) == 0 {
					continue
				}
				if prev >= 0 && !compatible(events[prev], e, c) {
					valid = false
					break
				}
				w := priorityWeight(e, events)
				for k := range score {
					score[k] += w[k]
				}
				if prev >= 0 && events[prev].Player != e.Player {
					score[4]--
				}
				prev = i
			}
			if valid && better(route{score: score}, route{score: best}) {
				best = score
			}
		}
		result := Plan(events, nil, c, 0)
		score := [5]int{}
		prev := ""
		for _, e := range result {
			if e.Status != "SCHEDULED" {
				continue
			}
			w := priorityWeight(e, events)
			for k := range score {
				score[k] += w[k]
			}
			if prev != "" && prev != e.Player {
				score[4]--
			}
			prev = e.Player
		}
		if score != best {
			t.Fatalf("trial %d got %v want %v", trial, score, best)
		}
		rng.Shuffle(len(events), func(i, j int) { events[i], events[j] = events[j], events[i] })
		if !reflect.DeepEqual(chosen(result), chosen(Plan(events, nil, c, 0))) {
			t.Fatal("arrival order affects selection")
		}
	}
}

func TestHalfSecondPostKillWindow(t *testing.T) {
	c := testConfig()
	c.Guard = 0
	start, end := bounds(ev("kill", "A", 10000), c)
	if start != 9000 || end != 10500 {
		t.Fatalf("window %d..%d", start, end)
	}
	c.Setup = 0
	c.Transition = 0
	if !compatible(ev("a", "A", 10000), ev("b", "B", 11500), c) {
		t.Fatal("camera held beyond half-second tail")
	}
}
