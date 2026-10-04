package replay

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeTrackBoundariesAndMapping(t *testing.T) {
	pts := []TrackPoint{{1000, [3]float64{0, 0, 0}, "projectile"}, {1100, [3]float64{10, 20, 30}, "projectile"}, {1200, [3]float64{20, 40, 60}, "effect"}, {2500, [3]float64{20, 40, 60}, "effect"}}
	p, d, phase, err := sampleTrack(pts, 1050)
	if err != nil || p != [3]float64{5, 10, 15} || d != [3]float64{10, 20, 30} || phase != "projectile" {
		t.Fatal(p, d, phase, err)
	}
	_, _, phase, err = sampleTrack(pts, 1200)
	if err != nil || phase != "effect" {
		t.Fatal(phase, err)
	}
	if _, _, _, err = sampleTrack(pts, 999); err == nil {
		t.Fatal("extrapolated before measured flight")
	}
	if _, _, _, err = sampleTrack(pts, 2501); err == nil {
		t.Fatal("extrapolated past known effect")
	}
	e := Event{Time: 1150, Utility: &Utility{Track: pts}}
	m := MapSourceEvent(e, 10000, .1)
	if m.Time != 11150 || m.Utility.Track[0].Time != 11000 || e.Utility.Track[0].Time != 1000 {
		t.Fatal("mapping mutates source or misses trajectory")
	}
	for name, track := range map[string][]TrackPoint{
		"gap":       {{0, [3]float64{}, "projectile"}, {251, [3]float64{}, "projectile"}},
		"reverse":   {{100, [3]float64{}, "effect"}, {200, [3]float64{}, "projectile"}},
		"duplicate": {{100, [3]float64{}, "projectile"}, {100, [3]float64{}, "effect"}},
	} {
		t.Run(name, func(t *testing.T) {
			if validateTrack(track) == nil {
				t.Fatal("accepted invalid trace")
			}
		})
	}
}

func TestNativeCameraRequiresRenderedReadback(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, e := ln.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				scan := bufio.NewScanner(conn)
				pose := ""
				for scan.Scan() {
					line := scan.Text()
					switch {
					case strings.HasPrefix(line, "spec_goto "):
						pose = line
					case line == "spec_pos":
						if valid {
							fmt.Fprintln(conn, pose)
						} else {
							fmt.Fprintln(conn, "Can't use cheat command spec_pos")
						}
					case strings.HasPrefix(line, "echo "):
						fmt.Fprintln(conn, strings.TrimPrefix(line, "echo "))
					}
				}
			}()
			a, err := New(t.TempDir(), "director")
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			c := a.s.Config
			c.NetCon = ln.Addr().String()
			c.CalibratedUntil = nowMS() + 10000
			a.s.Config = c
			now := nowMS()
			u := &Utility{Kind: "hegrenade", ID: "throw-1", Evidence: "test entity", Track: []TrackPoint{{now, [3]float64{20, 30, 40}, "effect"}, {now + 3000, [3]float64{20, 30, 40}, "effect"}}}
			j := Job{ID: "job", Epoch: c.Epoch, Utility: u, Events: []Event{{Utility: u}}, Start: now + 500, End: now + 2500}
			cam, err := a.prepareNativeCamera(j, c)
			if valid {
				if err != nil {
					t.Fatal(err)
				}
				if cam.Check() != nil || len(cam.Evidence()) == 0 {
					t.Fatal("missing readback evidence")
				}
				a.mu.Lock()
				a.s.Config.Epoch++
				a.mu.Unlock()
				time.Sleep(80 * time.Millisecond)
				if cam.Check() == nil {
					t.Fatal("accepted invalidated epoch")
				}
				cam.Close()
				cam.Close()
			} else if err == nil {
				cam.Close()
				t.Fatal("accepted console echo without actual position")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("console connection leaked")
			}
		})
	}
}

func TestMergedUtilityRejectsConflictingSamples(t *testing.T) {
	e := Event{Player: "p", Match: "m", Map: "map", Epoch: 1, Utility: &Utility{Kind: "hegrenade", ID: "throw", Evidence: "test", Track: []TrackPoint{{100, [3]float64{1, 2, 3}, "effect"}, {200, [3]float64{1, 2, 3}, "effect"}}}}
	b := jsonCopy(e)
	b.Utility.Track[0].Position[0] = 99
	if _, err := mergedUtility([]Event{e, b}); err == nil {
		t.Fatal("conflicting positions accepted")
	}
	b = jsonCopy(e)
	b.Utility.Track[1].Time = 300
	u, err := mergedUtility([]Event{e, b})
	if err != nil || len(u.Track) != 3 {
		t.Fatal(u, err)
	}
}

// Opt-in real CS2 fixture test. The large match Demo is not redistributed.
func TestRealDemoUtilityAttribution(t *testing.T) {
	path := os.Getenv("REPLAY_TEST_DEMO")
	if path == "" {
		t.Skip("set REPLAY_TEST_DEMO to the documented validation Demo")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var es []Event
	err = ParseSource(context.Background(), f, "", SourceOptions{Match: "proof", SourceID: "proof", Epoch: 1, FromTick: 14633, ToTick: 105843}, func(e Event) error {
		if e.Utility != nil {
			es = append(es, e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 4 || es[0].Utility == nil || es[0].Utility.ID != "proof-14529-2027" || es[0].Player != "76561198064211003" || es[0].Victim != "76561198434196430" {
		t.Fatalf("unexpected association: %+v", es)
	}
	if !nativeTrackReady(es[0], defaults()) {
		t.Fatal("real measured trace cannot cover two-second window")
	}
	if es[1].Utility.ID != "proof-76235-651" || es[1].Utility.Track[0].Phase != "projectile" {
		t.Fatal("molotov impact was confused with inferno")
	}
	if nativeTrackReady(es[1], defaults()) {
		t.Fatal("short flight must not fabricate pre-throw coordinates")
	}
	if es[3].Utility.ID != "proof-fire-105688-1558" || es[3].Utility.Track[0].Phase != "effect" || !nativeTrackReady(es[3], defaults()) {
		t.Fatal("wrong fire attribution or insufficient coverage")
	}
}
