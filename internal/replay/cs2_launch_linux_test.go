package replay

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestHeadlessLaunchPort(t *testing.T) {
	for _, s := range []string{"127.0.0.1:2121", "[::1]:2121", "localhost:2121"} {
		if p, e := launchPort(s); e != nil || p != 2121 {
			t.Fatal(s, p, e)
		}
	}
	for _, s := range []string{"192.168.0.114:2121", "127.0.0.1:22", "localhost:99999", "localhost:bad"} {
		if _, e := launchPort(s); e == nil {
			t.Fatal(s)
		}
	}
}
func TestSteamLaunchFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steam.pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = sendSteamLaunch(path, 2211); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4096)
	n, err := f.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(b[:n])
	if !strings.Contains(payload, "'-applaunch' '730'") || !strings.Contains(payload, "'-netconport' '2211'") || !strings.Contains(payload, "'+cl_demo_predict' '0'") || !strings.Contains(payload, "'+spec_show_xray' '1'") || !strings.HasSuffix(payload, "\n") {
		t.Fatal(payload)
	}
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(path, link)
	if err = sendSteamLaunch(link, 2121); err == nil {
		t.Fatal("symlink allowed")
	}
	regular := filepath.Join(t.TempDir(), "regular")
	os.WriteFile(regular, []byte("unchanged"), 0600)
	if err = sendSteamLaunch(regular, 2121); err == nil {
		t.Fatal("regular file allowed")
	}
	b, _ = os.ReadFile(regular)
	if string(b) != "unchanged" {
		t.Fatal("file modified")
	}
}
func TestHeadlessStartGuards(t *testing.T) {
	a := testService(t, "director")
	if err := a.startHeadlessCS2(); err == nil {
		t.Fatal("director allowed")
	}
	a.role = "agent"
	a.cs2Launch.Phase = "starting"
	if err := a.startHeadlessCS2(); err == nil {
		t.Fatal("duplicate allowed")
	}
	if w := request(t, a, "POST", "/api/cs2/start", map[string]any{}, "", ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	a.cs2Launch.Phase = ""
	a.localDemo.Phase = "armed"
	if err := a.startHeadlessCS2(); err == nil {
		t.Fatal("armed test interrupted")
	}
}
