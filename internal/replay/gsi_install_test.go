package replay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverSteamConfig(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "snap/steam/common/.local/share/Steam")
	cfg := filepath.Join(root, "steamapps/common/Counter-Strike Global Offensive/game/csgo/cfg")
	if err := os.MkdirAll(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := findCS2Config(home)
	if err != nil || got != cfg {
		t.Fatal(got, err)
	}
	other := filepath.Join(home, "library")
	otherCfg := filepath.Join(other, "steamapps/common/Counter-Strike Global Offensive/game/csgo/cfg")
	os.MkdirAll(otherCfg, 0700)
	os.WriteFile(filepath.Join(root, "steamapps/libraryfolders.vdf"), []byte(`"libraryfolders" { "1" { "path" "`+other+`" } }`), 0600)
	if _, err = findCS2Config(home); err == nil {
		t.Fatal("ambiguous libraries accepted")
	}
	os.RemoveAll(cfg)
	if got, err = findCS2Config(home); err != nil || got != otherCfg {
		t.Fatal(got, err)
	}
}
func TestInstallAgentGSI(t *testing.T) {
	a := testService(t, "agent")
	dir := t.TempDir()
	other := filepath.Join(dir, "gamestate_integration_other.cfg")
	os.WriteFile(other, []byte("keep"), 0600)
	path, err := a.installAgentGSI(dir, "0.0.0.0:7799")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "http://127.0.0.1:7799/gsi/b") || strings.Contains(string(data), "auth") {
		t.Fatal("incorrect GSI config")
	}
	before, _ := os.Stat(path)
	if _, err = a.installAgentGSI(dir, "0.0.0.0:7799"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("rewrote identical config")
	}
	if _, err = a.installAgentGSI(dir, "192.168.0.162:7788"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), "auth") || !strings.Contains(string(data), "192.168.0.162:7788") {
		t.Fatal("stale endpoint or unexpected authentication")
	}
	data, _ = os.ReadFile(other)
	if string(data) != "keep" {
		t.Fatal("modified other GSI")
	}
	if _, err = a.installAgentGSI(filepath.Join(dir, "missing"), "127.0.0.1:7788"); err == nil {
		t.Fatal("missing directory accepted")
	}
}

func TestDirectorGSIUsesAWithoutPairing(t *testing.T) {
	a := testService(t, "director")
	path, err := a.installLocalGSI(t.TempDir(), "127.0.0.1:7788")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if filepath.Base(path) != "gamestate_integration_replay_a.cfg" || !strings.Contains(string(data), "/gsi/a") || strings.Contains(string(data), "auth") {
		t.Fatal("incorrect A GSI")
	}
}
