package replay

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

func (a *Service) startHotkeys() {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		user := syscall.NewLazyDLL("user32.dll")
		register := user.NewProc("RegisterHotKey")
		unregister := user.NewProc("UnregisterHotKey")
		peek := user.NewProc("PeekMessageW")
		type point struct{ X, Y int32 }
		type message struct {
			Hwnd    uintptr
			Message uint32
			WParam  uintptr
			LParam  uintptr
			Time    uint32
			Point   point
			Private uint32
		}
		var msg message
		current := "\x00"
		registered := false
		defer func() {
			if registered {
				unregister.Call(0, 1)
			}
		}()
		timer := time.NewTicker(50 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-timer.C:
				a.mu.Lock()
				wanted := a.s.Output.Hotkey
				a.mu.Unlock()
				if wanted != current {
					if registered {
						unregister.Call(0, 1)
						registered = false
					}
					current = wanted
					mods, key, e := parseHotkey(wanted)
					status := "全局快捷键已关闭"
					if e == nil && key != 0 {
						ok, _, _ := register.Call(0, 1, uintptr(mods|0x4000), uintptr(key))
						registered = ok != 0
						if registered {
							status = "全局快捷键已注册：" + wanted
						} else {
							status = "快捷键注册失败（可能被占用），请更换组合"
						}
					}
					a.mu.Lock()
					a.hotkeyStatus = status
					a.mu.Unlock()
				}
				for {
					ok, _, _ := peek.Call(uintptr(unsafe.Pointer(&msg)), 0, 0x0312, 0x0312, 1)
					if ok == 0 {
						break
					}
					if msg.WParam == 1 {
						a.triggerHotkey()
					}
				}
			}
		}
	}()
}
