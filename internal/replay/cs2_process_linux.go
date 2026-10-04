package replay

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func cs2Command(command []byte) bool {
	first := string(bytes.SplitN(command, []byte{0}, 2)[0])
	// FEX may expose /proc/PID/exe as "exe"; its argv[0] retains the game path.
	return strings.HasSuffix(first, "/game/bin/linuxsteamrt64/cs2") || strings.HasSuffix(first, "/game/bin/linux64/cs2")
}
func cs2Process(pid int) bool {
	root := fmt.Sprintf("/proc/%d", pid)
	st, err := os.Stat(root)
	if err != nil {
		return false
	}
	info, ok := st.Sys().(*syscall.Stat_t)
	if !ok || info.Uid != uint32(os.Getuid()) {
		return false
	}
	cmd, err := os.ReadFile(filepath.Join(root, "cmdline"))
	return err == nil && cs2Command(cmd)
}
func processExited(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(stat), ')')
	return end >= 0 && len(stat) > end+2 && (stat[end+2] == 'Z' || stat[end+2] == 'X')
}
func killCS2Process(pid int) error {
	// os.FindProcess uses a pidfd on supported Linux kernels, pinning identity
	// before checking argv and signalling, rather than using broad pkill matching.
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer p.Release()
	if !cs2Process(pid) {
		return nil
	}
	if err = p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	for i := 0; i < 40; i++ {
		if processExited(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("CS2 进程 %d 尚未退出，请检查系统状态", pid)
}
func terminateCS2() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := []int{}
	var failures []error
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !cs2Process(pid) {
			continue
		}
		if err = killCS2Process(pid); err != nil {
			failures = append(failures, fmt.Errorf("进程 %d：%w", pid, err))
		} else {
			pids = append(pids, pid)
		}
	}
	return pids, errors.Join(failures...)
}
