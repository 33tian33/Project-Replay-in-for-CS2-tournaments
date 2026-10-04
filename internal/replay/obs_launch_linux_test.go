package replay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareOBSWebsocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin", "config.json")
	url, password, err := prepareOBSWebsocket(path, false)
	if err != nil || url != "ws://127.0.0.1:4466" || len(password) < 32 {
		t.Fatalf("prepare failed: %s %v", url, err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0600 {
		t.Fatal("password config permissions")
	}
	raw, _ := os.ReadFile(path)
	cfg := obsINI(string(raw))
	cfg["ServerPort"] = "4477"
	cfg["CustomSetting"] = "preserved"
	raw = []byte("[General]\nUnrelated=yes\n" + updateOBSINI(string(raw), cfg))
	os.WriteFile(path, raw, 0600)
	url, p2, err := prepareOBSWebsocket(path, false)
	if err != nil || url != "ws://127.0.0.1:4477" || password != p2 {
		t.Fatal("existing settings not preserved", err)
	}
	backup, _ := os.ReadFile(path + ".replay-backup")
	if string(backup) != string(raw) {
		t.Fatal("backup not preserved")
	}
	before, _ := os.ReadFile(path)
	if !strings.Contains(string(before), "CustomSetting") || !strings.Contains(string(before), "[General]\nUnrelated=yes") {
		t.Fatal("lost unknown setting")
	}
	_, _, err = prepareOBSWebsocket(path, true)
	after, _ := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("running configuration changed", err)
	}
}

func TestPrepareOBSRejectsInvalidRunningConfig(t *testing.T) {
	for _, raw := range []string{``, `[OBSWebSocket]
ServerEnabled=false
ServerPort=4466`, `[OBSWebSocket]
ServerEnabled=true
ServerPort=4466
AuthRequired=true`} {
		path := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(path, []byte(raw), 0600)
		if _, _, err := prepareOBSWebsocket(path, true); err == nil {
			t.Fatal("accepted invalid running configuration", raw)
		}
		got, _ := os.ReadFile(path)
		if string(got) != raw {
			t.Fatal("modified invalid config")
		}
	}
}

func TestOBSStartGuards(t *testing.T) {
	d := testService(t, "director")
	if w := request(t, d, "POST", "/api/obs/start", map[string]any{}, "", ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	a := testService(t, "agent")
	a.active = true
	if err := a.startHeadlessOBS(); err == nil {
		t.Fatal("allowed start during capture")
	}
	a.active = false
	a.obsLaunch = CS2Launch{Phase: "starting"}
	if w := request(t, a, "POST", "/api/obs/start", map[string]any{}, "", ""); w.Code != 409 {
		t.Fatal("duplicate start", w.Code)
	}
	if w := request(t, a, "GET", "/api/state", nil, "", ""); w.Code != 200 {
		t.Fatal("state inaccessible")
	}
	if err := a.configure(a.s.Config); err == nil {
		t.Fatal("allowed config changes while starting")
	}
}
