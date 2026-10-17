package replay

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParsePlatformGOTV(t *testing.T) {
	for _, tc := range []struct {
		raw, address, password string
		hasPassword            bool
	}{
		{"127.0.0.1:27020", "127.0.0.1:27020", "", false},
		{"connect example.com:27020;password xxx", "example.com:27020", "xxx", true},
		{" connect 192.168.0.2：27020; password Abc!_123; ", "192.168.0.2:27020", "Abc!_123", true},
		{`connect host:27020;password "with spaces"`, "host:27020", "with spaces", true},
		{`connect host:27020;password ""`, "host:27020", "", true},
		{"CONNECT [::1]:27020;PASSWORD secret", "[::1]:27020", "secret", true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			address, password, err := parseGOTV(tc.raw)
			if err != nil || address != tc.address || (password != nil) != tc.hasPassword {
				t.Fatal(address, password, err)
			}
			if password != nil && *password != tc.password {
				t.Fatal("password mismatch")
			}
		})
	}
	for _, raw := range []string{"", "host:0", "host:65536", "connect host:123;quit", "connect host:123;password test;quit", "connect host:123\nquit", `connect host:123;password "x\";quit"`, "connect host:123;password xxx xxx", "connect host:123;password", "connect host:123;password \"bad\npass\""} {
		if _, _, err := parseGOTV(raw); err == nil {
			t.Fatalf("accepted invalid instruction %q", raw)
		}
	}
}
func waitGOTVCommand(t *testing.T, commands func() []string, want string) {
	t.Helper()
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		for _, line := range commands() {
			if line == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected connection command not received")
}
func TestDirectorForwardsPlatformInstructionAndClearsOldPassword(t *testing.T) {
	b := demoService(t, "agent")
	var commands func() []string
	b.s.Config.NetCon, commands = demoConsole(t)
	server := httptest.NewServer(b.Handler())
	defer server.Close()
	a := testService(t, "director")
	a.s.Config.Mode = "live"
	a.s.Config.WorkerURL = server.URL
	w := request(t, a, "POST", "/api/connect-gotv", gotvRequest{GOTV: "connect game.example:27020;password test-secret"}, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	waitGOTVCommand(t, commands, `spec_show_xray 1; password "test-secret"; connect game.example:27020`)
	for _, s := range []*Service{a, b} {
		if s.s.Config.GOTV != "game.example:27020" || s.s.Config.GOTVPassword != "test-secret" {
			t.Fatal("parsed settings not retained")
		}
		if strings.Contains(request(t, s, "GET", "/api/state", nil, "", "").Body.String(), "test-secret") {
			t.Fatal("state leaked password")
		}
	}
	w = request(t, a, "POST", "/api/connect-gotv", gotvRequest{GOTV: "other.example:27020"}, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	waitGOTVCommand(t, commands, `spec_show_xray 1; password ""; connect other.example:27020`)
	if b.s.Config.GOTVPassword != "" {
		t.Fatal("old server password leaked into new address")
	}
}
func TestSavePlatformCommandAndReconnectKeepsPassword(t *testing.T) {
	b := demoService(t, "agent")
	server := httptest.NewServer(b.Handler())
	defer server.Close()
	a := testService(t, "director")
	c := a.s.Config
	c.Mode = "live"
	c.WorkerURL = server.URL
	c.AutoSendGOTV = true
	c.GOTV = "connect game.example:27020;password secret-test"
	w := request(t, a, "POST", "/api/config", c, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if b.s.Config.GOTVPassword != "secret-test" || a.s.Config.GOTV != "game.example:27020" {
		t.Fatal("save did not parse and forward password")
	}
	// A redacted settings form must preserve the saved password on a normal save.
	c = jsonCopy(a.s.Config)
	c.GOTVPassword = ""
	w = request(t, a, "POST", "/api/config", c, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if b.s.Config.GOTVPassword != "secret-test" {
		t.Fatal("redacted config save cleared password")
	}
	w = request(t, a, "POST", "/api/connect-gotv", gotvRequest{}, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if b.s.Config.GOTVPassword != "secret-test" {
		t.Fatal("reconnect lost password")
	}
	c.GOTV = `connect game.example:27020;password ""`
	w = request(t, a, "POST", "/api/config", c, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if b.s.Config.GOTVPassword != "" || a.s.Config.GOTVPassword != "" {
		t.Fatal("explicit password clear failed")
	}
}
func TestInvalidGOTVDoesNotChangeConfiguration(t *testing.T) {
	a := testService(t, "director")
	old := a.s.Config
	c := old
	c.GOTV = "connect host:27020;password x;quit"
	if err := a.configure(c); err == nil {
		t.Fatal("accepted extra command")
	}
	if a.s.Config.Epoch != old.Epoch || a.s.Config.GOTV != old.GOTV {
		t.Fatal("invalid command changed settings")
	}
}

func TestForcedGOTVReconnectCancelsAndWaitsForCamera(t *testing.T) {
	b := demoService(t, "agent")
	var commands func() []string
	b.s.Config.NetCon, commands = demoConsole(t)
	b.active = true
	b.s.Jobs = []Job{{ID: "inflight", Status: "CAPTURING", End: nowMS() - 1}, {ID: "finished", Status: "READY"}}
	b.s.Events = []Event{{ID: "event", JobID: "inflight", Status: "CAPTURING"}}
	b.localDemo = LocalDemo{Phase: "armed", Due: nowMS() + 1000}
	b.refreshPlayerMappingLocked(false)
	b.playerMapping.Slots["player"] = 14
	before := b.s.Config.Epoch
	b.captureMu.Lock()
	locked := true
	defer func() {
		if locked {
			b.captureMu.Unlock()
		}
	}()
	w := request(t, b, "POST", "/api/connect-gotv", gotvRequest{GOTV: "connect game.example:27020;password secret", Force: true}, "", "")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	b.mu.Lock()
	if b.s.Config.Epoch != before+1 || b.s.Jobs[0].Status != "CANCELLED" || b.s.Jobs[1].Status != "READY" || b.localDemo.Phase != "" || len(b.playerMapping.Slots) != 0 {
		t.Error("old session not invalidated")
	}
	b.mu.Unlock()
	if got := b.captureSnapshot(Job{ID: "inflight"}, true); got.Status != "CANCELLED" {
		t.Fatal("cancellation overwritten", got.Status)
	}
	b.jobUpdate("inflight", "READY", "late result", nil)
	if len(commands()) != 0 {
		t.Fatal("reconnect did not wait for camera release")
	}
	b.captureMu.Unlock()
	locked = false
	waitGOTVCommand(t, commands, `disconnect; spec_show_xray 1; password "secret"; connect game.example:27020`)
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		b.mu.Lock()
		phase := b.gotvReconnect.Phase
		b.mu.Unlock()
		if phase == "sent" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("reconnect not completed")
}

func TestDirectorCanReconnectWhileBusy(t *testing.T) {
	b := demoService(t, "agent")
	var commands func() []string
	b.s.Config.NetCon, commands = demoConsole(t)
	server := httptest.NewServer(b.Handler())
	defer server.Close()
	a := testService(t, "director")
	a.active = true
	a.localDemo = LocalDemo{Phase: "running"}
	a.s.Config.WorkerURL = server.URL
	// Even demo mode on A must not prevent an explicit B reconnect.
	w := request(t, a, "POST", "/api/connect-gotv", gotvRequest{GOTV: `password "abc def"; connect game.example:27020`, Force: true}, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	waitGOTVCommand(t, commands, `disconnect; spec_show_xray 1; password "abc def"; connect game.example:27020`)
	if strings.Contains(w.Body.String(), "abc def") {
		t.Fatal("response exposed password")
	}
}

func TestInvalidForcedGOTVDoesNotCancelCapture(t *testing.T) {
	a := testService(t, "agent")
	a.s.Jobs = []Job{{ID: "active", Status: "CAPTURING"}}
	epoch := a.s.Config.Epoch
	w := request(t, a, "POST", "/api/connect-gotv", gotvRequest{GOTV: "host:27020;quit", Force: true}, "", "")
	if w.Code != 400 || a.s.Config.Epoch != epoch || a.s.Jobs[0].Status != "CAPTURING" {
		t.Fatal("invalid input disrupted recording")
	}
}

func TestDirectorReadsBReconnectStatus(t *testing.T) {
	b := testService(t, "agent")
	b.gotvReconnect = CS2Launch{Phase: "sent", Message: "GOTV 重连命令已发送，等待新的比赛 GSI"}
	server := httptest.NewServer(b.Handler())
	defer server.Close()
	a := testService(t, "director")
	a.s.Config.WorkerURL = server.URL
	w := request(t, a, "GET", "/api/gotv-status", nil, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"phase":"sent"`) {
		t.Fatal(w.Body.String())
	}
}

func TestNormalGOTVConnectReplacesOldStatus(t *testing.T) {
	b := demoService(t, "agent")
	b.s.Config.Mode = "live"
	b.s.Config.NetCon, _ = demoConsole(t)
	b.gotvReconnect = CS2Launch{Phase: "receiving", Message: "old connection"}
	w := request(t, b, "POST", "/api/connect-gotv", gotvRequest{GOTV: "game.example:27020"}, "", "")
	if w.Code != 200 || b.gotvReconnect.Phase != "sent" {
		t.Fatal("new command retained old connection status", w.Body.String(), b.gotvReconnect)
	}
}
