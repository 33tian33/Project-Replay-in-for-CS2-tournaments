package replay

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

func TestHeadlessCookieSelectsOnlyLocalDisplay20(t *testing.T) {
	var b bytes.Buffer
	record := func(family uint16, host, display, cookie string) {
		binary.Write(&b, binary.BigEndian, family)
		for _, field := range []string{host, display, "MIT-MAGIC-COOKIE-1", cookie} {
			binary.Write(&b, binary.BigEndian, uint16(len(field)))
			b.WriteString(field)
		}
	}
	record(256, "host", "0", "0000000000000000")
	record(256, "other", "20", "1111111111111111")
	if _, err := headlessCookie(b.Bytes(), "host"); err == nil {
		t.Fatal("accepted another desktop or host")
	}
	record(256, "host", "20", "2222222222222222")
	got, err := headlessCookie(b.Bytes(), "host")
	if err != nil || got != hex.EncodeToString([]byte("2222222222222222")) {
		t.Fatal("wrong cookie selection", err)
	}
	if _, err := headlessCookie(b.Bytes()[:b.Len()-1], "host"); err == nil {
		t.Fatal("accepted truncated authority")
	}
	if headlessCS2PID(os.Getpid()) {
		t.Fatal("test process is not CS2")
	}
}

// Opt-in test on the dedicated desktop: creates two temporary windows, verifies
// real window-manager behavior, then removes them and restores the prior focus.
func TestHeadlessWindowGuardWithWindowManager(t *testing.T) {
	if os.Getenv("REPLAY_TEST_HEADLESS_X11") != "1" {
		t.Skip("requires dedicated :20 X11 session")
	}
	x, err := openHeadlessX11()
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	root := xproto.Setup(x).DefaultScreen(x).Root
	atom := func(name string) xproto.Atom {
		r, e := xproto.InternAtom(x, false, uint16(len(name)), name).Reply()
		if e != nil {
			t.Fatal(e)
		}
		return r.Atom
	}
	property := func(w xproto.Window, name string) []uint32 {
		r, e := xproto.GetProperty(x, false, w, atom(name), xproto.GetPropertyTypeAny, 0, 4096).Reply()
		if e != nil {
			t.Fatal(e)
		}
		var values []uint32
		for i := 0; i+4 <= len(r.Value); i += 4 {
			values = append(values, xgb.Get32(r.Value[i:]))
		}
		return values
	}
	send := func(w xproto.Window, name string, data ...uint32) {
		values := make([]uint32, 5)
		copy(values, data)
		event := xproto.ClientMessageEvent{Format: 32, Window: w, Type: atom(name), Data: xproto.ClientMessageDataUnionData32New(values)}
		if e := xproto.SendEventChecked(x, false, root, xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify, string(event.Bytes())).Check(); e != nil {
			t.Fatal(e)
		}
	}
	old := property(root, "_NET_ACTIVE_WINDOW")
	if len(old) == 1 {
		defer func() { send(xproto.Window(old[0]), "_NET_ACTIVE_WINDOW", 2, 0) }()
	}
	makeWindow := func(pid uint32) xproto.Window {
		w, e := xproto.NewWindowId(x)
		if e != nil {
			t.Fatal(e)
		}
		if e = xproto.CreateWindowChecked(x, 0, w, root, 0, 0, 100, 100, 0, xproto.WindowClassInputOutput, 0, 0, nil).Check(); e != nil {
			t.Fatal(e)
		}
		data := make([]byte, 4)
		xgb.Put32(data, pid)
		xproto.ChangeProperty(x, xproto.PropModeReplace, w, atom("_NET_WM_PID"), xproto.AtomCardinal, 32, 1, data)
		name := []byte("Replay window protection test")
		xproto.ChangeProperty(x, xproto.PropModeReplace, w, xproto.AtomWmName, xproto.AtomString, 8, uint32(len(name)), name)
		xproto.MapWindow(x, w)
		x.Sync()
		return w
	}
	game := makeWindow(uint32(os.Getpid()))
	defer xproto.DestroyWindow(x, game)
	blocker := makeWindow(uint32(os.Getpid() + 1))
	defer xproto.DestroyWindow(x, blocker)
	owns := func(pid int) bool { return pid == os.Getpid() }
	wait := func(f func() bool) {
		until := time.Now().Add(700 * time.Millisecond)
		for time.Now().Before(until) {
			if f() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("window manager did not apply protection")
	}
	wait(func() bool {
		v := property(root, "_NET_CLIENT_LIST_STACKING")
		for _, id := range v {
			if id == uint32(game) {
				return true
			}
		}
		return false
	})
	verify := func() bool {
		n, e := protectCS2Windows(x, owns)
		if e != nil {
			t.Fatal(e)
		}
		if n != 1 {
			t.Fatalf("protected %d windows", n)
		}
		states := property(game, "_NET_WM_STATE")
		above := false
		for _, a := range states {
			above = above || a == uint32(atom("_NET_WM_STATE_ABOVE"))
		}
		stack := property(root, "_NET_CLIENT_LIST_STACKING")
		active := property(root, "_NET_ACTIVE_WINDOW")
		return above && len(stack) > 0 && stack[len(stack)-1] == uint32(game) && len(active) == 1 && active[0] == uint32(game)
	}
	wait(verify)
	send(blocker, "_NET_WM_STATE", 1, uint32(atom("_NET_WM_STATE_ABOVE")), 0, 2)
	send(blocker, "_NET_ACTIVE_WINDOW", 2, 0)
	wait(func() bool { v := property(root, "_NET_ACTIVE_WINDOW"); return len(v) == 1 && v[0] == uint32(blocker) })
	wait(verify)
	send(game, "WM_CHANGE_STATE", 3) // ICCCM IconicState.
	wait(func() bool {
		for _, state := range property(game, "_NET_WM_STATE") {
			if state == uint32(atom("_NET_WM_STATE_HIDDEN")) {
				return true
			}
		}
		return false
	})
	wait(verify)
}
