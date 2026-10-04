package replay

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHUDGuards(t *testing.T) {
	a := testService(t, "agent")
	for _, body := range []any{map[string]bool{"visible": true}, map[string]any{}} {
		if w := request(t, a, "POST", "/api/hud", body, "", ""); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	a.s.Config.Mode = "live"
	a.active = true
	if err := a.setHUD(false); err == nil {
		t.Fatal("allowed change during capture")
	}
	a.active = false
	a.role = "director"
	if err := a.setHUD(true); err == nil {
		t.Fatal("director controlled local HUD")
	}
}

func TestHUDCommands(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	a.s.Config.SessionLock = filepath.Join(t.TempDir(), "session.lock")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a.s.Config.NetCon = listener.Addr().String()
	received := make(chan string, 2)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			scanner := bufio.NewScanner(conn)
			visible := false
			first := true
			for scanner.Scan() {
				line := scanner.Text()
				if first {
					received <- line + "\n"
					first = false
					visible = strings.Contains(line, "cl_drawhud 1")
				}
				if line == "cl_drawhud; cl_draw_only_deathnotices" {
					fmt.Fprintf(conn, "cl_drawhud = %t\ncl_draw_only_deathnotices = false\n", visible)
				}
				if strings.HasPrefix(line, "echo ") {
					fmt.Fprintln(conn, strings.TrimPrefix(line, "echo "))
				}
			}
			conn.Close()
		}
	}()
	for _, visible := range []bool{false, true} {
		if err := a.setHUD(visible); err != nil {
			t.Fatal(err)
		}
		want := "cl_drawhud 0\n"
		if visible {
			want = fullHUDCommand + "\n"
		}
		select {
		case got := <-received:
			if got != want {
				t.Fatalf("got %q want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no console command")
		}
	}
}

func TestHUDRejectsUnappliedCommand(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a.s.Config.NetCon = listener.Addr().String()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "cl_drawhud; cl_draw_only_deathnotices" {
				fmt.Fprintln(conn, "cl_drawhud = false\ncl_draw_only_deathnotices = true")
			}
			if strings.HasPrefix(line, "echo ") {
				fmt.Fprintln(conn, strings.TrimPrefix(line, "echo "))
			}
		}
	}()
	if err := a.setHUD(true); err == nil || !strings.Contains(err.Error(), "未确认") {
		t.Fatal("unapplied HUD falsely reported success", err)
	}
}

func TestTeamHUDCommandReadback(t *testing.T) {
	for _, applied := range []bool{true, false} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			commands := make(chan string, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					line := scanner.Text()
					if line == teamHUDCommand {
						commands <- line
					}
					if strings.HasPrefix(line, "cl_drawhud;") {
						fmt.Fprintf(conn, "cl_drawhud = %t\ncrosshair = false\nspec_show_xray = true\ncl_drawhud_force_radar = -1\ncl_drawhud_force_teamid_overhead = -1\n", !applied)
					}
					if strings.HasPrefix(line, "echo ") {
						fmt.Fprintln(conn, strings.TrimPrefix(line, "echo "))
					}
				}
			}()
			err = hideGameUI(listener.Addr().String())
			if (err == nil) != applied {
				t.Fatalf("applied=%v err=%v", applied, err)
			}
			select {
			case <-commands:
			case <-time.After(time.Second):
				t.Fatal("missing CSStudio UI commands")
			}
		})
	}
}

func TestRecordingUsesTeamHUD(t *testing.T) {
	a := testService(t, "agent")
	if a.recordingHUDCommand() != fullHUDCommand {
		t.Fatal("default HUD")
	}
	a.s.TeamHUD = true
	if a.recordingHUDCommand() != teamHUDCommand {
		t.Fatal("recording restores native HUD")
	}
	a.saveLocked()
	b, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
	if err != nil || !strings.Contains(string(b), `"team_hud": true`) {
		t.Fatal("HUD preference not persisted", err, string(b))
	}
}
