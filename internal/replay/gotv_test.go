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
