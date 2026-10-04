package replay

import (
	"bufio"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func demoConsole(t *testing.T) (string, func() []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var commands []string
	var currentFile string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer c.Close()
				scan := bufio.NewScanner(c)
				for scan.Scan() {
					line := scan.Text()
					mu.Lock()
					commands = append(commands, line)
					mu.Unlock()
					if strings.HasPrefix(line, `cl_demo_predict 0; playdemo "`) {
						currentFile = strings.TrimSuffix(strings.TrimPrefix(line, `cl_demo_predict 0; playdemo "`), `"`)
					}
					if line == "demo_pause; demo_gototick" {
						fmt.Fprintf(c, "Currently playing 1000 of 100000 ticks. File:%s\n", currentFile)
					}
					if strings.HasPrefix(line, "echo ") {
						fmt.Fprintln(c, strings.TrimPrefix(line, "echo "))
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	return ln.Addr().String(), func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), commands...) }
}
func demoService(t *testing.T, role string) *Service {
	a := testService(t, role)
	a.s.Config.Mode = "live"
	a.s.Config.NetCon, _ = demoConsole(t)
	a.s.Config.SessionLock = filepath.Join(t.TempDir(), "session.lock")
	return a
}
func prepareDemo(t *testing.T, a *Service, path string) {
	t.Helper()
	for _, p := range []demoRequest{{Action: "load", Path: path}, {Action: "seek", Tick: 1000}, {Action: "ready"}} {
		if err := a.demoAction(p); err != nil {
			t.Fatal(p.Action, err)
		}
	}
}
func TestLocalDemoPairDelayAndCancellation(t *testing.T) {
	a, b := demoService(t, "director"), demoService(t, "agent")
	server := httptest.NewServer(b.Handler())
	defer server.Close()
	a.s.Config.WorkerURL = server.URL
	path := filepath.Join(t.TempDir(), "same demo.dem")
	if err := os.WriteFile(path, []byte("test demo fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	prepareDemo(t, a, path)
	prepareDemo(t, b, path)
	if err := a.startDemoPair(12.5); err != nil {
		t.Fatal(err)
	}
	if a.localDemo.Phase != "armed" || b.localDemo.Phase != "armed" || b.localDemo.Due-a.localDemo.Due-a.remoteOffset != 12500 {
		t.Fatal(a.localDemo, b.localDemo)
	}
	if !a.s.Config.Paused || !b.s.Config.Paused || a.s.Config.CalibratedUntil != 0 {
		t.Fatal("invented calibration")
	}
	if err := a.configure(a.s.Config); err == nil {
		t.Fatal("changed armed session")
	}
	a.mu.Lock()
	a.localDemo.Due = nowMS()
	a.tickDemoLocked()
	a.mu.Unlock()
	if a.localDemo.Phase != "running" || a.localDemo.SentAt == 0 {
		t.Fatal(a.localDemo)
	}
	b.mu.Lock()
	b.tickDemoLocked()
	b.mu.Unlock()
	if b.localDemo.Phase != "armed" {
		t.Fatal("B resumed early")
	}
	if w := request(t, a, "POST", "/api/local-demo", demoRequest{Action: "stop-pair"}, "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	b.mu.Lock()
	b.tickDemoLocked()
	b.mu.Unlock()
	if a.localDemo.Phase != "stopped" || b.localDemo.Phase != "stopped" {
		t.Fatal("cancellation did not disarm both")
	}
}
func TestLocalDemoRejectsMismatchAndInvalidInput(t *testing.T) {
	for _, p := range []string{"", `x.dem;quit`, "x\n.dem", `x".dem`, "x.mp4"} {
		if _, err := demoPath(p); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	a, b := demoService(t, "director"), demoService(t, "agent")
	server := httptest.NewServer(b.Handler())
	defer server.Close()
	a.s.Config.WorkerURL = server.URL
	a.localDemo = LocalDemo{Phase: "ready", SHA256: "one", Tick: 1000}
	b.localDemo = LocalDemo{Phase: "ready", SHA256: "two", Tick: 1000}
	if err := a.startDemoPair(8); err == nil {
		t.Fatal("accepted different demo")
	}
	if w := request(t, a, "POST", "/api/local-demo", demoRequest{Action: "stop"}, "", ""); w.Code != 200 {
		t.Fatal("direct stop failed", w.Body.String())
	}
	a.localDemo.Phase = "armed"
	a.localDemo.Due = nowMS() - 1000
	a.tickDemoLocked()
	if a.localDemo.Phase != "failed" {
		t.Fatal("started late")
	}
	a.localDemo = LocalDemo{Phase: "armed", RunID: "new", Due: nowMS() + 10000}
	if err := a.demoAction(demoRequest{Action: "stop", RunID: "old"}); err == nil || a.localDemo.Phase != "armed" {
		t.Fatal("stale stop cancelled new session")
	}
	a.active = true
	if err := a.demoAction(demoRequest{Action: "stop"}); err == nil {
		t.Fatal("interrupted recording")
	}
}
func TestLocalDemoReadyRequiresActualTickAndOldEventsStayCancelled(t *testing.T) {
	a := demoService(t, "director")
	a.localDemo = LocalDemo{Phase: "seeking", Tick: 2000}
	if err := a.demoAction(demoRequest{Action: "ready"}); err == nil {
		t.Fatal("accepted wrong tick")
	}
	a.s.Events = []Event{ev("old", "player", nowMS()+60000)}
	a.resetDemoTimelineLocked()
	c := a.s.Config
	c.Mode = "demo"
	c.Paused = false
	if got := Plan(a.s.Events, nil, c, nowMS()); got[0].Status != "CANCELLED" {
		t.Fatal(got)
	}
}
