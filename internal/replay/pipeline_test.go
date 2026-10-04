package replay

import (
	"bufio"
	"net"
	"testing"
	"time"
)

func TestAgentQueuesDisjointWindows(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Paused = false
	a.s.Config.CalibratedUntil = nowMS() + 60000
	start := nowMS() + 10000
	makeJob := func(id string, start int64) Job {
		e := ev(id+"event", "A", start+1200)
		return Job{ID: id, Epoch: 1, Match: e.Match, Map: e.Map, Player: e.Player, Demo: true, Start: start, End: start + 2400, Events: []Event{e}}
	}
	if _, err := a.acceptJob(makeJob("first", start)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.acceptJob(makeJob("second", start+3000)); err != nil {
		t.Fatal("disjoint job blocked by pending work:", err)
	}
	if _, err := a.acceptJob(makeJob("overlap", start+1000)); err == nil {
		t.Fatal("overlapping job accepted")
	}
	if len(a.s.Jobs) != 2 {
		t.Fatal(a.s.Jobs)
	}
}

func TestPlannerDoesNotReserveCameraForEncoding(t *testing.T) {
	c := testConfig()
	e := ev("next", "B", 10000)
	for _, status := range []string{"FINALIZING", "TRANSFERRING"} {
		jobs := []Job{{Start: 8800, End: 11200, Status: status}}
		if len(chosen(Plan([]Event{e}, jobs, c, 0))) != 1 {
			t.Fatal(status, "blocked camera")
		}
	}
}

func TestPlayerSwitchDoesNotWaitForGSI(t *testing.T) {
	a := testService(t, "agent")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	commands := make(chan string, 2)
	go func() {
		for i := 0; i < 2; i++ {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			c.Close()
			commands <- line
		}
	}()
	c := a.s.Config
	c.NetCon = listener.Addr().String()
	c.Mappings["new-player"] = 14
	a.observed = "old-player"
	a.observedAt = nowMS()
	j := Job{Epoch: c.Epoch, Player: "new-player", Slot: 14, Start: nowMS() + 5000}
	done := make(chan cameraMonitor, 1)
	go func() { m, _ := a.prepareCamera(j, c); done <- m }()
	var m cameraMonitor
	select {
	case m = <-done:
	case <-time.After(time.Second):
		t.Fatal("switch waited for GSI before recording")
	}
	if m == nil {
		t.Fatal("camera setup failed")
	}
	defer m.Close()
	if got := <-commands; got != "spec_autodirector 0; spec_mode 1\n" {
		t.Fatal(got)
	}
	if got := <-commands; got != "spec_player 14; "+fullHUDCommand+"\n" {
		t.Fatal(got)
	}
	// No stale observation may validate a clip once the protected window starts.
	pc := m.(*playerCamera)
	pc.job.Start = nowMS() - 1
	if err := pc.Check(); err == nil {
		t.Fatal("accepted wrong target")
	}
	a.mu.Lock()
	a.observed = "new-player"
	a.observedAt = pc.switchAt + 1
	a.mu.Unlock()
	if err := pc.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectorDispatchesWhileEarlierJobPending(t *testing.T) {
	a := testService(t, "director")
	a.s.Config.Paused = false
	a.s.Jobs = []Job{{ID: "encoding", Status: "FINALIZING"}}
	a.active = true
	a.s.Events = []Event{ev("next", "B", nowMS()+2200)}
	a.tick()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.s.Jobs) != 2 || a.s.Events[0].JobID == "" {
		t.Fatal("pending processing blocked dispatch", a.s.Events)
	}
}
