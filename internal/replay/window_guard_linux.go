package replay

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

func (a *Service) startHeadlessWindowGuard() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		lastError := ""
		for {
			count, err := ensureHeadlessCS2Windows()
			status := CS2Launch{Phase: "waiting", Message: "无头窗口保护：等待 :20 上的 CS2"}
			if count > 0 {
				status = CS2Launch{Phase: "ready", Message: "CS2 无头窗口置顶保护已开启"}
			}
			if err != nil {
				status = CS2Launch{Phase: "failed", Message: "无头窗口置顶暂不可用：" + err.Error()}
			}
			a.mu.Lock()
			a.headlessWindow = status
			if err != nil && lastError != err.Error() {
				a.logLocked("warn", status.Message)
			}
			a.mu.Unlock()
			if err != nil {
				lastError = err.Error()
			} else {
				lastError = ""
			}
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Match the real game executable and its display, never a title such as Steam's
// "Counter-Strike 2" store page. Never activate a window on the user's desktop.
func headlessCS2PID(pid int) bool {
	if !cs2Process(pid) {
		return false
	}
	env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	for _, entry := range strings.Split(string(env), "\x00") {
		if entry == "DISPLAY=:20" || entry == "DISPLAY=:20.0" {
			return true
		}
	}
	return false
}

func headlessCookie(raw []byte, hostname string) (string, error) {
	r := bytes.NewReader(raw)
	for r.Len() > 0 {
		var family uint16
		if err := binary.Read(r, binary.BigEndian, &family); err != nil {
			return "", err
		}
		fields := make([]string, 4)
		for i := range fields {
			var size uint16
			if err := binary.Read(r, binary.BigEndian, &size); err != nil {
				return "", err
			}
			value := make([]byte, size)
			if _, err := io.ReadFull(r, value); err != nil {
				return "", err
			}
			fields[i] = string(value)
		}
		if (family == 65535 || family == 256 && fields[0] == hostname) && (fields[1] == "20" || fields[1] == "") && fields[2] == "MIT-MAGIC-COOKIE-1" && len(fields[3]) == 16 {
			return hex.EncodeToString([]byte(fields[3])), nil
		}
	}
	return "", errors.New("未找到 :20 的 X11 授权记录")
}

func openHeadlessX11() (*xgb.Conn, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	paths := []string{filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "cs2-headless.Xauthority"), os.Getenv("XAUTHORITY"), filepath.Join(home, ".Xauthority")}
	cookie := ""
	for _, path := range paths {
		if path == "" {
			continue
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			continue
		}
		cookie, e = headlessCookie(raw, hostname)
		if e == nil {
			break
		}
	}
	if cookie == "" {
		return nil, errors.New("无法读取无头桌面 :20 的 Xauthority")
	}
	conn, err := net.DialTimeout("unix", "/tmp/.X11-unix/X20", time.Second)
	if err != nil {
		return nil, err
	}
	// Bound handshake and all requests, so a stopped X server cannot block shutdown.
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	x, err := xgb.NewConnNetWithCookieHex(conn, cookie)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return x, nil
}

func ensureHeadlessCS2Windows() (int, error) {
	if _, err := os.Stat("/tmp/.X11-unix/X20"); os.IsNotExist(err) {
		return 0, nil
	}
	x, err := openHeadlessX11()
	if err != nil {
		return 0, err
	}
	defer x.Close()
	return protectCS2Windows(x, headlessCS2PID)
}

func protectCS2Windows(x *xgb.Conn, owns func(int) bool) (int, error) {
	root := xproto.Setup(x).DefaultScreen(x).Root
	atoms := map[string]xproto.Atom{}
	for _, name := range []string{"_NET_CLIENT_LIST_STACKING", "_NET_WM_PID", "_NET_WM_STATE", "_NET_WM_STATE_ABOVE", "_NET_WM_STATE_BELOW", "_NET_WM_STATE_HIDDEN", "_NET_ACTIVE_WINDOW", "_NET_RESTACK_WINDOW"} {
		reply, err := xproto.InternAtom(x, false, uint16(len(name)), name).Reply()
		if err != nil {
			return 0, err
		}
		atoms[name] = reply.Atom
	}
	property := func(w xproto.Window, name string) ([]uint32, error) {
		p, err := xproto.GetProperty(x, false, w, atoms[name], xproto.GetPropertyTypeAny, 0, 4096).Reply()
		if err != nil {
			return nil, err
		}
		values := []uint32{}
		if p.Format == 32 {
			for i := 0; i+4 <= len(p.Value); i += 4 {
				values = append(values, xgb.Get32(p.Value[i:]))
			}
		}
		return values, nil
	}
	send := func(w xproto.Window, name string, data []uint32) error {
		values := make([]uint32, 5)
		copy(values, data)
		e := xproto.ClientMessageEvent{Format: 32, Window: w, Type: atoms[name], Data: xproto.ClientMessageDataUnionData32New(values)}
		return xproto.SendEventChecked(x, false, root, xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify, string(e.Bytes())).Check()
	}
	windows, err := property(root, "_NET_CLIENT_LIST_STACKING")
	if err != nil {
		return 0, err
	}
	active, err := property(root, "_NET_ACTIVE_WINDOW")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range windows {
		w := xproto.Window(id)
		pids, e := property(w, "_NET_WM_PID")
		if e != nil || len(pids) != 1 || !owns(int(pids[0])) {
			continue
		}
		states, e := property(w, "_NET_WM_STATE")
		if e != nil {
			return count, e
		}
		has := func(name string) bool {
			for _, state := range states {
				if state == uint32(atoms[name]) {
					return true
				}
			}
			return false
		}
		if has("_NET_WM_STATE_BELOW") {
			if e = send(w, "_NET_WM_STATE", []uint32{0, uint32(atoms["_NET_WM_STATE_BELOW"]), 0, 2}); e != nil {
				return count, e
			}
		}
		if !has("_NET_WM_STATE_ABOVE") {
			if e = send(w, "_NET_WM_STATE", []uint32{1, uint32(atoms["_NET_WM_STATE_ABOVE"]), 0, 2}); e != nil {
				return count, e
			}
		}
		if has("_NET_WM_STATE_HIDDEN") {
			if e = xproto.MapWindowChecked(x, w).Check(); e != nil {
				return count, e
			}
		}
		if len(active) != 1 || active[0] != id {
			if e = send(w, "_NET_ACTIVE_WINDOW", []uint32{2, 0}); e != nil {
				return count, e
			}
		}
		if len(windows) > 0 && windows[len(windows)-1] != id {
			if e = send(w, "_NET_RESTACK_WINDOW", []uint32{2, 0, xproto.StackModeAbove}); e != nil {
				return count, e
			}
		}
		count++
	}
	return count, nil
}
