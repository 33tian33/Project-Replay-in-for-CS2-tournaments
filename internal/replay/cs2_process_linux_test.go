package replay

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestCS2CommandMatching(t *testing.T) {
	for _, s := range []string{"/games/Counter-Strike Global Offensive/game/bin/linuxsteamrt64/cs2\x00-steam", "/games/game/bin/linux64/cs2\x00"} {
		if !cs2Command([]byte(s)) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"/usr/bin/steam\x00-applaunch\x00730", "/usr/bin/obs\x00cs2", "/usr/bin/python\x00/games/game/bin/linuxsteamrt64/cs2", "/games/game/bin/linuxsteamrt64/cs2.sh"} {
		if cs2Command([]byte(s)) {
			t.Fatal(s)
		}
	}
}
func TestCS2KillHelper(t *testing.T) {
	if os.Getenv("REPLAY_KILL_HELPER") != "1" {
		return
	}
	fmt.Println("ready")
	for {
		time.Sleep(time.Second)
	}
}
func TestCS2KillExactProcess(t *testing.T) {
	for _, game := range []bool{false, true} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCS2KillHelper$")
		cmd.Env = append(os.Environ(), "REPLAY_KILL_HELPER=1")
		if game {
			cmd.Args[0] = "/test/game/bin/linuxsteamrt64/cs2"
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if _, err = bufio.NewReader(out).ReadString('\n'); err != nil {
			cmd.Process.Kill()
			t.Fatal(err)
		}
		err = killCS2Process(cmd.Process.Pid)
		exited := processExited(cmd.Process.Pid)
		cmd.Process.Kill()
		cmd.Wait()
		if err != nil || exited != game {
			t.Fatalf("game=%v exited=%v err=%v", game, exited, err)
		}
	}
}
func TestCloseCS2Guards(t *testing.T) {
	a := testService(t, "director")
	if w := request(t, a, "POST", "/api/cs2/close", map[string]any{}, "", ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if _, err := a.closeCS2(); err == nil {
		t.Fatal("director allowed")
	}
	a.role = "agent"
	a.active = true
	a.localDemo = LocalDemo{Phase: "armed", Due: nowMS() + 1000}
	if _, err := a.closeCS2(); err == nil || a.localDemo.Phase != "armed" {
		t.Fatal("active capture interrupted")
	}
}
