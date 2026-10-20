package replay

import "testing"

func TestAgentRestartDefaultsToAstraAndKeepsPreviousSourceForCleanup(t *testing.T) {
	for _, mode := range []string{"openhud", "url", "package", "builtin"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			a, err := New(dir, "agent")
			if err != nil {
				t.Fatal(err)
			}
			a.s.HUD = HUDSettings{Mode: mode, URL: "http://localhost:1350/", Source: "Old HUD", Width: 1280, Height: 720}
			a.s.HUDActiveSource = "Old HUD"
			a.Close()
			b, err := New(dir, "agent")
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if b.s.HUD.Mode != "astra" || b.s.HUD.Width != 1920 || b.s.HUD.URL != "" || b.s.HUDActiveSource != "Old HUD" {
				t.Fatalf("unexpected startup HUD: %+v, source %q", b.s.HUD, b.s.HUDActiveSource)
			}
		})
	}
}
