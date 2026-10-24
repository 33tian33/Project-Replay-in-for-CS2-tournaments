package replay

import (
	"runtime"
	"testing"
	"time"
)

func TestGOTVHUDDelay(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux recorder only")
	}
	for _, mode := range []string{"send", "reconnect", "closed"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			a := testService(t, "agent")
			address, commands := demoConsole(t)
			a.s.Config.Mode = "live"
			a.s.Config.NetCon = address
			a.gotvSentAt = nowMS()
			a.gotvReconnect = CS2Launch{Phase: "sent"}
			p := gsiFrame(0, "live")
			p["player"] = map[string]any{"steamid": "76561198000000001"}
			p["allplayers"] = map[string]any{"76561198000000001": map[string]any{}}
			start := time.Now()
			if err := a.ingestGSI("b", p); err != nil {
				t.Fatal(err)
			}
			if mode == "reconnect" {
				a.mu.Lock()
				a.gotvSentAt++
				a.gotvReconnect.Phase = "starting"
				a.mu.Unlock()
			}
			if mode == "closed" {
				a.Close()
			}
			for time.Since(start) < 5500*time.Millisecond {
				if mode != "closed" {
					if err := a.ingestGSI("b", p); err != nil {
						t.Fatal(err)
					}
				}
				lines := commands()
				if len(lines) > 0 && time.Since(start) < 5*time.Second {
					t.Fatal("HUD sent before five seconds")
				}
				time.Sleep(25 * time.Millisecond)
			}
			lines := commands()
			if mode == "send" {
				if len(lines) != 1 || lines[0] != teamHUDCommand {
					t.Fatalf("expected exactly one hide command: %v", lines)
				}
				a.mu.Lock()
				hidden := a.nativeHUDHidden
				a.mu.Unlock()
				if !hidden {
					t.Fatal("recording would restore native UI")
				}
			} else if len(lines) != 0 {
				t.Fatalf("obsolete timer sent commands: %v", lines)
			}
		})
	}
}
