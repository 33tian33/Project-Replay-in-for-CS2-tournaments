package replay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type steamProcess struct {
	pid, ppid int
	args      []string
	display   string
	group     string
}

func isSteamClient(args []string) bool {
	if len(args) == 0 {
		return false
	}
	first := strings.ReplaceAll(args[0], "\\", "/")
	return strings.HasSuffix(first, "/ubuntu12_32/steam") || first == "steam"
}
func steamSnapshot() ([]steamProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := []steamProcess{}
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil {
			continue
		}
		root := filepath.Join("/proc", entry.Name())
		st, e := os.Stat(root)
		if e != nil {
			continue
		}
		info, ok := st.Sys().(*syscall.Stat_t)
		if !ok || info.Uid != uint32(os.Getuid()) {
			continue
		}
		data, e := os.ReadFile(filepath.Join(root, "cmdline"))
		if e != nil || len(data) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		stat, e := os.ReadFile(filepath.Join(root, "stat"))
		if e != nil {
			continue
		}
		end := strings.LastIndexByte(string(stat), ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(stat)[end+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		env, e := os.ReadFile(filepath.Join(root, "environ"))
		display := ""
		if e == nil {
			for _, v := range strings.Split(string(env), "\x00") {
				if strings.HasPrefix(v, "DISPLAY=") {
					display = strings.TrimPrefix(v, "DISPLAY=")
				}
			}
		}
		cg, _ := os.ReadFile(filepath.Join(root, "cgroup"))
		out = append(out, steamProcess{pid, ppid, args, display, string(cg)})
	}
	return out, nil
}
func desktopSteam(processes []steamProcess) bool {
	for _, p := range processes {
		if isSteamClient(p.args) && p.display != ":20" && p.display != ":20.0" {
			return true
		}
	}
	return false
}
func steamShutdownTargets(processes []steamProcess) (map[int]bool, error) {
	targets := map[int]bool{}
	groups := map[string]bool{}
	for _, p := range processes {
		// Do not terminate games as a side effect of moving the Steam client.
		if len(p.args) > 0 && strings.Contains(p.args[0], "/steamapps/common/") && !strings.Contains(p.args[0], "/SteamLinuxRuntime_") {
			return nil, errors.New("检测到 Steam 游戏仍在运行，请先关闭游戏再将 Steam 切换到无头")
		}
		if isSteamClient(p.args) {
			targets[p.pid] = true
			if strings.Contains(p.group, "snap.steam.steam-") {
				groups[p.group] = true
			}
		}
	}
	for _, p := range processes {
		if groups[p.group] {
			targets[p.pid] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, p := range processes {
			if targets[p.ppid] && !targets[p.pid] {
				targets[p.pid] = true
				changed = true
			}
		}
	}
	return targets, nil
}
func moveSteamHeadless(ctx context.Context) error {
	snapshot, err := steamSnapshot()
	if err != nil {
		return err
	}
	targets, err := steamShutdownTargets(snapshot)
	if err != nil {
		return err
	}
	processes := map[int]*os.Process{}
	defer func() {
		for _, p := range processes {
			p.Release()
		}
	}()
	// Pin identities before stopping the service so PID reuse cannot target others.
	for pid := range targets {
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		processes[pid] = p
	}
	if err = exec.CommandContext(ctx, "systemctl", "--user", "stop", "cs2-headless-steam.service").Run(); err != nil {
		return fmt.Errorf("停止旧 Steam 服务失败：%w", err)
	}
	for _, p := range processes {
		if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		live := false
		for pid := range processes {
			if !processExited(pid) {
				live = true
			}
		}
		if !live {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	for pid, p := range processes {
		if !processExited(pid) {
			if err = p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return err
			}
		}
	}
	for i := 0; i < 20; i++ {
		left := false
		for pid := range processes {
			if !processExited(pid) {
				left = true
			}
		}
		if !left {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("旧 Steam 尚未完全退出，请关闭后重试")
}
