package replay

import (
	"fmt"
	"strconv"
	"strings"
)

func parseHotkey(s string) (uint32, uint32, error) {
	if s == "" {
		return 0, 0, nil
	}
	parts := strings.Split(strings.ToUpper(s), "+")
	var mods uint32
	for _, p := range parts[:len(parts)-1] {
		switch p {
		case "CTRL":
			mods |= 2
		case "ALT":
			mods |= 1
		case "SHIFT":
			mods |= 4
		default:
			return 0, 0, fmt.Errorf("快捷键支持 Ctrl、Alt、Shift 与字母、数字或 F1–F11")
		}
	}
	key := parts[len(parts)-1]
	var vk uint32
	if len(key) == 1 && (key[0] >= 'A' && key[0] <= 'Z' || key[0] >= '0' && key[0] <= '9') {
		vk = uint32(key[0])
	} else if strings.HasPrefix(key, "F") {
		n, _ := strconv.Atoi(key[1:])
		if n >= 1 && n <= 11 {
			vk = uint32(111 + n)
		}
	}
	if vk == 0 || mods == 0 {
		return 0, 0, fmt.Errorf("请使用带 Ctrl、Alt 或 Shift 的快捷键，例如 Ctrl+Alt+R")
	}
	return mods, vk, nil
}
func (a *Service) triggerHotkey() {
	a.mu.Lock()
	defer a.mu.Unlock()
	round := a.gsiRound["a"] + 1
	if a.endRound > 0 && stringField(obj(a.previous["a"], "round"), "phase") == "over" {
		round = a.endRound
	}
	if e := a.manualOutputLocked("round", round); e != nil {
		a.outputError = e.Error()
		a.logLocked("warn", "快捷键播出："+e.Error())
	}
}
