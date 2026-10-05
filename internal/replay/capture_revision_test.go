package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	dem "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
)

func TestSaveKillsStayInRoundUntilNextFreeze(t *testing.T) {
	for _, increment := range []bool{false, true} {
		t.Run(map[bool]string{false: "counter updates later", true: "counter updates at win"}[increment], func(t *testing.T) {
			a := testService(t, "director")
			a.s.Config.Mode = "live"
			frame := func(raw, kills int, phase string, remaining float64) map[string]any {
				p := clockGSI(raw+1, kills, phase, remaining)
				p["round"] = map[string]any{"phase": phase}
				obj(obj(p, "allplayers"), "A")["state"] = map[string]any{"round_kills": float64(kills)}
				return p
			}
			a.ingestGSI("a", frame(4, 0, "live", 20))
			a.ingestGSI("a", frame(4, 1, "bomb", 40))
			epoch := a.s.Config.Epoch
			raw := 4
			if increment {
				raw = 5
			}
			a.ingestGSI("a", frame(raw, 1, "over", 7))
			a.ingestGSI("a", frame(5, 2, "over", 6.5))
			a.ingestGSI("a", frame(5, 3, "over", 3))
			if len(a.s.Events) != 3 {
				t.Fatalf("save kills lost: %+v", a.s.Events)
			}
			for _, e := range a.s.Events {
				if e.Round != 5 || e.Clock.Round != 5 {
					t.Fatalf("kill moved to next round: %+v", e)
				}
			}
			save := a.s.Events[2]
			if save.Clock.Phase != "over" || save.ClockAnchor == nil || save.ClockAnchor.Phase != "bomb" || save.Kills != 3 {
				t.Fatalf("save clock/count: %+v", save)
			}
			if a.s.Config.Epoch != epoch || a.gsiRound["a"]+1 != 5 || a.output.Round != 5 {
				t.Fatal("clock/output round mismatch", a.output, a.gsiRound)
			}
			var hud struct {
				Round int `json:"round"`
			}
			json.Unmarshal(request(t, a, "GET", "/hud-api/state", nil, "", "").Body.Bytes(), &hud)
			if hud.Round != 5 {
				t.Fatal("HUD changed round early", hud)
			}
			a.ingestGSI("a", frame(5, 0, "freezetime", 15))
			a.ingestGSI("a", frame(5, 1, "live", 114))
			if len(a.s.Events) != 4 || a.s.Events[3].Round != 6 || a.s.Events[3].Kills != 1 || a.s.Events[3].ClockAnchor != nil {
				t.Fatalf("next round not reset: %+v", a.s.Events)
			}
		})
	}
}

func TestRealDemoPostWinKillsUseEndingRound(t *testing.T) {
	path := os.Getenv("REPLAY_TEST_DEMO")
	if path == "" {
		t.Skip("set REPLAY_TEST_DEMO to verify post-win kills in a real match")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := dem.NewParser(f)
	defer p.Close()
	endingRound := 0
	postWin := false
	want := map[string]int{}
	p.RegisterEventHandler(func(events.RoundStart) { endingRound = p.GameState().TotalRoundsPlayed() + 1; postWin = false })
	p.RegisterEventHandler(func(events.RoundEnd) { postWin = true })
	p.RegisterEventHandler(func(e events.Kill) {
		if postWin && endingRound > 0 && !p.GameState().IsWarmupPeriod() && e.Killer != nil && e.Victim != nil && e.Weapon != nil {
			want[fmt.Sprintf("saveproof-%d-%d", p.GameState().IngameTick(), e.Victim.SteamID64)] = endingRound
		}
	})
	if err := p.ParseToEnd(); err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatal("fixture has no post-win kills")
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	checked := 0
	err = ParseSource(context.Background(), f, "", SourceOptions{Match: "proof", SourceID: "saveproof", Epoch: 1}, func(e Event) error {
		if round, ok := want[e.ID]; ok {
			checked++
			if e.Round != round {
				return fmt.Errorf("post-win kill %s: round %d, want %d", e.ID, e.Round, round)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked != len(want) {
		t.Fatalf("checked %d/%d post-win kills", checked, len(want))
	}
	t.Logf("verified %d post-win kills against real round boundaries", checked)
}

func revisionOBS(t *testing.T, handle func(string, map[string]any) map[string]any) string {
	t.Helper()
	up := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteJSON(map[string]any{"op": 0, "d": map[string]any{}})
		var hello any
		if c.ReadJSON(&hello) != nil {
			return
		}
		c.WriteJSON(map[string]any{"op": 2, "d": map[string]any{}})
		for {
			var req map[string]any
			if c.ReadJSON(&req) != nil {
				return
			}
			d := obj(req, "d")
			data := handle(stringField(d, "requestType"), obj(d, "requestData"))
			c.WriteJSON(map[string]any{"op": 7, "d": map[string]any{"requestId": d["requestId"], "requestStatus": map[string]any{"result": true, "code": 100}, "responseData": data}})
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func TestHUDDisablesOldZIPDuplicatesAndNestedOverlays(t *testing.T) {
	row := func(name string, id int) map[string]any { return map[string]any{"sourceName": name, "sceneItemId": id} }
	group := row("Group", 5)
	group["isGroup"] = true
	nested := row("Nested", 6)
	nested["sourceType"] = "OBS_SOURCE_TYPE_SCENE"
	rows := map[string][]any{
		"Main":   {row("Project Replay ZIP HUD new", 1), row("Project Replay ZIP HUD old", 2), row("Project Replay Team HUD", 3), row("External", 4), group, nested, row("Project Replay ZIP HUD new", 7), row("Sponsor", 8)},
		"Group":  {row("Project Replay Custom HUD", 1), row("Project Replay ZIP HUD new", 2)},
		"Nested": {row("Project Replay ZIP HUD orphan", 1)},
	}
	var mu sync.Mutex
	enabled := map[string]bool{}
	c := testConfig()
	c.OBSURL = revisionOBS(t, func(kind string, args map[string]any) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		scene := stringField(args, "sceneName")
		switch kind {
		case "GetSceneItemList", "GetGroupSceneItemList":
			return map[string]any{"sceneItems": rows[scene]}
		case "SetSceneItemEnabled":
			enabled[fmt.Sprintf("%s:%d", scene, int(number(args, "sceneItemId")))], _ = args["sceneItemEnabled"].(bool)
		}
		return map[string]any{}
	})
	o, err := openOBS(c)
	if err != nil {
		t.Fatal(err)
	}
	defer o.close()
	for i := 0; i < 2; i++ {
		if err := enforceSingleHUD(o, "Main", "Project Replay ZIP HUD new", "External", 1); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !enabled["Main:1"] {
		t.Fatal("selected HUD disabled", enabled)
	}
	for _, key := range []string{"Main:2", "Main:3", "Main:4", "Main:7", "Group:1", "Group:2", "Nested:1"} {
		if value, known := enabled[key]; !known || value {
			t.Fatal("duplicate HUD visible", key, enabled)
		}
	}
	if _, changed := enabled["Main:8"]; changed {
		t.Fatal("unrelated sponsor changed")
	}
}

func TestCaptureDirectCutsUseOneOBSRecording(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	a := testService(t, "agent")
	c := a.s.Config
	c.Mode, c.FFmpeg, c.FFprobe = "live", ffmpeg, ffprobe
	c.Setup = .4
	c.SessionLock = filepath.Join(t.TempDir(), "capture.lock")
	c.Mappings = map[string]int{"A": 14, "B": 29}
	sample := filepath.Join(t.TempDir(), "raw.mp4")
	if err := runCommand(a.ctx, 15*time.Second, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=c=blue:s=256x144:r=30", "-t", "8", "-c:v", "libx264", "-pix_fmt", "yuv420p", sample); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c.NetCon = ln.Addr().String()
	var mu sync.Mutex
	var commands []string
	observed := "A"
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			conn.Close()
			mu.Lock()
			commands = append(commands, strings.TrimSpace(line))
			if strings.HasPrefix(line, "spec_player 14;") {
				observed = "A"
			}
			if strings.HasPrefix(line, "spec_player 29;") {
				observed = "B"
			}
			mu.Unlock()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	stop, feedDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(feedDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				mu.Lock()
				player := observed
				mu.Unlock()
				a.mu.Lock()
				a.observed, a.observedAt = player, nowMS()
				a.mu.Unlock()
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-feedDone })
	var starts, stops atomic.Int32
	var recordStart int64
	active := false
	c.OBSURL = revisionOBS(t, func(kind string, args map[string]any) map[string]any {
		switch kind {
		case "GetRecordStatus":
			return map[string]any{"outputActive": active, "outputDuration": max(int64(0), nowMS()-recordStart)}
		case "StartRecord":
			active = true
			recordStart = nowMS()
			starts.Add(1)
		case "StopRecord":
			active = false
			stops.Add(1)
			return map[string]any{"outputPath": sample}
		}
		return map[string]any{}
	})
	a.s.Config = c
	now := nowMS()
	es := []Event{ev("first", "A", now+2200), ev("second", "B", now+3500), ev("third", "A", now+4800)}
	start, _ := bounds(es[0], c)
	_, end := bounds(es[2], c)
	j := Job{ID: "direct-cuts", Epoch: c.Epoch, Match: c.Match, Map: c.Map, Player: "A", Events: es, Start: start, End: end, Status: "COMMITTED"}
	a.s.Jobs = []Job{j}
	arts, err := a.capture(j, c)
	if err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || stops.Load() != 1 || len(arts) != 3 {
		t.Fatal("not one complete capture", starts.Load(), stops.Load(), arts)
	}
	mu.Lock()
	defer mu.Unlock()
	var targets []string
	for _, cmd := range commands {
		if strings.Contains(cmd, "spec_next") || strings.Contains(cmd, "spec_prev") {
			t.Fatal("cycled through players", cmd)
		}
		if strings.HasPrefix(cmd, "spec_player ") {
			targets = append(targets, strings.Split(cmd, ";")[0])
		}
	}
	if !reflect.DeepEqual(targets, []string{"spec_player 14", "spec_player 29", "spec_player 14"}) {
		t.Fatal("wrong direct-cut sequence", targets)
	}
	for i, art := range arts {
		lo, hi := eventClipBounds(j, es[i], c)
		if art.Player != es[i].Player || art.Duration < float64(hi-lo)/1000-.05 || art.Duration > float64(hi-lo)/1000+.08 {
			t.Fatal("export disagrees with camera timeline", art)
		}
	}
}

func TestEarlySaveKillPreparesBeforeRoundOver(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	now := nowMS()
	e := clockEvent("save", 5, "over", 6.7)
	e.ClockAnchor = &RoundClock{5, "live", .2}
	e.ClockOffset = .5
	a.s.Events = []Event{e}
	a.roundClocks = map[string]clockSample{"b": {RoundClock{5, "live", 2}, now, now}}
	a.resolveRoundEventsLocked(now)
	if got := a.s.Events[0].Time - now; got != 2300 {
		t.Fatalf("early save predicted %d", got)
	}
	if got := Plan(a.s.Events, nil, a.s.Config, now); got[0].Status != "SCHEDULED" {
		t.Fatal(got)
	}
	if err := a.checkRoundJobLocked(Job{Events: a.s.Events}); err != nil {
		t.Fatal(err)
	}
	// The actual over countdown and the preceding-phase anchor resolve identically.
	a.roundClocks["b"] = clockSample{RoundClock{5, "over", 6.9}, now + 2100, now + 2100}
	a.resolveRoundEventsLocked(now + 2100)
	if got := a.s.Events[0].Time - now; got < 2299 || got > 2301 {
		t.Fatalf("phase jump changed time: %d", got)
	}
}

func TestRoundEndBoundaryKeepsCaptureProgress(t *testing.T) {
	a := testService(t, "agent")
	now := nowMS()
	a.previous["b"] = clockGSI(5, 0, "live", .1)
	a.gsiRound["b"] = 4
	a.roundClocks = map[string]clockSample{"b": {RoundClock{5, "live", .1}, now - 100, now - 100}}
	a.updateRoundClockLocked("b", clockGSI(6, 0, "over", 7), now)
	if s := a.roundClocks["b"]; s.Clock.Round != 5 || s.ProgressAt != now {
		t.Fatal("normal round end treated as pause", s)
	}
}

func TestContinuousKillsUseOneRecordingAndDirectShotTimeline(t *testing.T) {
	c := testConfig()
	es := []Event{ev("a1", "A", 10000), ev("a2", "A", 10400), ev("b", "B", 11600), ev("c", "C", 13000), ev("a3", "A", 14500)}
	if got := chosen(Plan(es, nil, c, 0)); len(got) != len(es) {
		t.Fatal("nearby kills dropped", got)
	}
	start, _ := bounds(es[0], c)
	_, end := bounds(es[len(es)-1], c)
	j := Job{Player: "A", Events: es, Start: start, End: end}
	shots := cameraShots(j, c)
	var players []string
	for _, shot := range shots {
		players = append(players, shot.Player)
		target, _ := cameraJob(j, c, shot.Switch)
		if target.Player != shot.Player {
			t.Fatal("incorrect direct target", target)
		}
	}
	if !reflect.DeepEqual(players, []string{"A", "B", "C", "A"}) {
		t.Fatal(players)
	}
	for _, e := range es {
		lo, hi := eventClipBounds(j, e, c)
		if lo > e.Time-cameraBeforeMS || hi < e.Time+cameraAfterMS || hi-lo > 1500 {
			t.Fatalf("kill clipped or wrong POV: %s %d..%d", e.ID, lo, hi)
		}
	}
	// A cluster of the same player's kills wins over a simultaneous distant fight.
	conflict := ev("other", "X", 10500)
	if got := chosen(Plan(append(es, conflict), nil, c, 0)); !reflect.DeepEqual(got, []string{"a1", "a2", "a3", "b", "c"}) {
		t.Fatal(got)
	}
}

func TestLateDifferentPlayerExtendsCaptureBeforeCut(t *testing.T) {
	a := testService(t, "agent")
	c := a.s.Config
	now := nowMS()
	first := ev("first", "A", now-400)
	next := ev("next", "B", now+800)
	start, end := bounds(first, c)
	a.s.Jobs = []Job{{ID: "one", Player: "A", Epoch: c.Epoch, Match: c.Match, Map: c.Map, Events: []Event{first}, Start: start, End: end, Status: "CAPTURING"}}
	a.s.Events = []Event{next}
	a.extendCapturesLocked(c, now)
	if a.s.Events[0].JobID != "one" || len(a.s.Jobs[0].Events) != 2 {
		t.Fatal("late direct cut not merged", a.s.Events, a.s.Jobs)
	}
	// A past cut must never be rewritten to claim a kill that was not observed.
	a.s.Events = []Event{ev("late", "C", now+850)}
	a.extendCapturesLocked(c, now+700)
	if a.s.Events[0].JobID != "" {
		t.Fatal("retroactive camera cut accepted")
	}
}

func TestContinuousJobDoesNotCrossRoundBoundary(t *testing.T) {
	a := testService(t, "agent")
	first, next := ev("save", "A", nowMS()+2000), ev("opening", "A", nowMS()+2500)
	first.Round, next.Round = 5, 6
	a.s.Events = []Event{first, next}
	a.tick()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.s.Jobs) != 1 || len(a.s.Jobs[0].Events) != 1 {
		t.Fatal("merged different rounds", a.s.Jobs)
	}
}

func TestSchedulerCommitsMixedPlayersAsOneTask(t *testing.T) {
	a := testService(t, "agent")
	now := nowMS()
	a.s.Events = []Event{ev("a", "A", now+2200), ev("b", "B", now+3500), ev("c", "A", now+4800)}
	a.tick()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.s.Jobs) != 1 || len(a.s.Jobs[0].Events) != 3 {
		t.Fatal("continuous kills split into tasks", a.s.Jobs)
	}
	for _, e := range a.s.Events {
		if e.JobID != a.s.Jobs[0].ID {
			t.Fatal("event not linked to shared recording", e)
		}
	}
}

func TestPhaseAnchorRejectedAcrossRoundsAndMaps(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	e := clockEvent("save", 5, "over", 6)
	e.ClockAnchor = &RoundClock{4, "live", 1}
	e.ClockOffset = 2
	if _, err := a.acceptRoundEvent(e); err == nil {
		t.Fatal("cross-round clock anchor accepted")
	}
	e.ClockAnchor.Round = 5
	e.ClockAnchor.Phase = "over"
	if _, err := a.acceptRoundEvent(e); err == nil {
		t.Fatal("same-phase anchor accepted")
	}
	p, old := clockGSI(6, 0, "over", 6), clockGSI(5, 0, "live", 2)
	obj(p, "map")["name"] = "de_nuke"
	if roundNumber(p, old, 99) != 5 {
		t.Fatal("new map inherited old round latch")
	}
}

func TestAgentRoundDisplayMatchesClockAfterOverOutage(t *testing.T) {
	a := testService(t, "agent")
	a.ingestGSI("b", clockGSI(5, 0, "live", 2))
	a.gsiSeen["b"] = nowMS() - 6000
	// The score counter has not incremented yet on this over snapshot.
	a.ingestGSI("b", clockGSI(5, 0, "over", 7))
	if a.roundClocks["b"].Clock.Round != 5 || a.snapshot()["current_round"] != 5 {
		t.Fatal("round changed while resetting clock baseline", a.roundClocks)
	}
}
