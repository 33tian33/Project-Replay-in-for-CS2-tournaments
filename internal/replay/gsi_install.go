package replay

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var steamLibraryPath = regexp.MustCompile(`"path"\s+"([^"]+)"`)

func findCS2Config(home string) (string, error) {
	roots := []string{
		filepath.Join(home, ".steam/steam"), filepath.Join(home, ".steam/root"),
		filepath.Join(home, ".local/share/Steam"),
		filepath.Join(home, "snap/steam/common/.local/share/Steam"),
		filepath.Join(home, ".var/app/com.valvesoftware.Steam/.local/share/Steam"),
	}
	if runtime.GOOS == "windows" {
		roots = windowsSteamRoots()
	}
	return findCS2InLibraries(roots)
}

func findCS2InLibraries(roots []string) (string, error) {
	libraries := append([]string(nil), roots...)
	for _, root := range roots {
		data, err := os.ReadFile(filepath.Join(root, "steamapps/libraryfolders.vdf"))
		if err != nil {
			continue
		}
		for _, m := range steamLibraryPath.FindAllSubmatch(data, -1) {
			libraries = append(libraries, strings.ReplaceAll(string(m[1]), `\\`, `\`))
		}
	}
	found := map[string]bool{}
	for _, root := range libraries {
		path := filepath.Join(root, "steamapps/common/Counter-Strike Global Offensive/game/csgo/cfg")
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		if st, err := os.Stat(real); err == nil && st.IsDir() {
			found[real] = true
		}
	}
	if len(found) == 0 {
		return "", errors.New("未找到 CS2 cfg 目录；可用 -cs2-cfg 指定 game/csgo/cfg 的绝对路径")
	}
	if len(found) > 1 {
		return "", errors.New("找到多个 CS2 安装，请用 -cs2-cfg 指定本次使用的 game/csgo/cfg 目录")
	}
	for path := range found {
		return path, nil
	}
	return "", errors.New("未找到 CS2")
}

// InstallAgentGSI writes only Replay's dedicated B config. Failure is reported
// to the UI and does not prevent starting the application.
func (a *Service) InstallAgentGSI(directory, listenAddress string) error {
	return a.InstallLocalGSI(directory, listenAddress)
}

func (a *Service) InstallLocalGSI(directory, listenAddress string) error {
	side := "A"
	if a.role == "agent" {
		side = "B"
	}
	path, err := a.installLocalGSI(directory, listenAddress)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.logLocked("warn", "自动安装 "+side+" 路 GSI 失败："+err.Error())
	} else {
		a.logLocked("info", side+" 路 GSI 已就绪："+path+"；若 CS2 已运行，请重启游戏以加载配置")
	}
	a.saveLocked()
	return err
}
func (a *Service) installAgentGSI(directory, listenAddress string) (string, error) {
	if a.role != "agent" {
		return "", errors.New("自动安装 B 路 GSI 仅用于 Agent")
	}
	return a.installLocalGSI(directory, listenAddress)
}

func (a *Service) installLocalGSI(directory, listenAddress string) (string, error) {
	side := "a"
	if a.role == "agent" {
		side = "b"
	}
	if directory == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		directory, err = findCS2Config(home)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(directory) {
		return "", errors.New("-cs2-cfg 必须是绝对路径")
	}
	st, err := os.Stat(directory)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errors.New("CS2 cfg 路径不是目录")
	}
	host, port, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return "", err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	content := []byte(a.gsiConfig(side, "http://"+net.JoinHostPort(host, port)))
	path := filepath.Join(directory, "gamestate_integration_replay_"+side+".cfg")
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return path, nil
	}
	// Write in the target directory and atomically replace only our own file.
	f, err := os.CreateTemp(directory, ".replay-gsi-*")
	if err != nil {
		return "", fmt.Errorf("无法写入 %s: %w", directory, err)
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err = replaceFile(temporary, path); err != nil {
		return "", err
	}
	return path, nil
}
