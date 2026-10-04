package replay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const headlessOBSUnit = "cs2-headless-obs.service"

// OBS 30's WebSocket plugin stores its settings in global.ini.
// Preserve unrelated sections and keys, and never rewrite a running instance.
func obsINI(raw string) map[string]string {
	values := map[string]string{}
	section := ""
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if section != "[OBSWebSocket]" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return values
}
func updateOBSINI(raw string, values map[string]string) string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	result := []string{}
	section := ""
	written := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			section = trim
			result = append(result, line)
			if section == "[OBSWebSocket]" {
				for _, k := range keys {
					result = append(result, k+"="+values[k])
				}
				written = true
			}
			continue
		}
		if section == "[OBSWebSocket]" {
			if k, _, ok := strings.Cut(trim, "="); ok {
				if _, exists := values[strings.TrimSpace(k)]; exists {
					continue
				}
			}
		}
		result = append(result, line)
	}
	if !written {
		result = append(result, "[OBSWebSocket]")
		for _, k := range keys {
			result = append(result, k+"="+values[k])
		}
	}
	return strings.Join(result, "\n") + "\n"
}
func prepareOBSWebsocket(path string, running bool) (string, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	settings := obsINI(string(raw))
	port, _ := strconv.Atoi(settings["ServerPort"])
	password := strings.NewReplacer(`\\`, `\`, `\r`, "\r", `\n`, "\n").Replace(settings["ServerPassword"])
	validPort := port >= 1024 && port <= 65535
	if running {
		if settings["ServerEnabled"] != "true" || !validPort || settings["AuthRequired"] != "false" && password == "" {
			return "", "", errors.New("无头 OBS 已运行但 WebSocket 配置无效，请先停止该 OBS 服务再重试")
		}
	} else {
		if !validPort {
			port = 4466
		}
		if password == "" {
			secret := make([]byte, 24)
			if _, err := rand.Read(secret); err != nil {
				return "", "", err
			}
			password = hex.EncodeToString(secret)
		}
		settings["ServerEnabled"], settings["AuthRequired"], settings["FirstLoad"] = "true", "true", "false"
		settings["ServerPort"], settings["ServerPassword"], settings["AlertsEnabled"] = strconv.Itoa(port), strings.NewReplacer(`\`, `\\`, "\r", `\r`, "\n", `\n`).Replace(password), "false"
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", "", err
		}
		if len(raw) > 0 {
			backup, err := os.OpenFile(path+".replay-backup", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil && !os.IsExist(err) {
				return "", "", err
			}
			if err == nil {
				_, e := backup.Write(raw)
				ce := backup.Close()
				if e != nil {
					return "", "", e
				}
				if ce != nil {
					return "", "", ce
				}
			}
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".replay-obs-*")
		if err != nil {
			return "", "", err
		}
		defer os.Remove(tmp.Name())
		_, err = tmp.WriteString(updateOBSINI(string(raw), settings))
		ce := tmp.Close()
		if err != nil {
			return "", "", err
		}
		if ce != nil {
			return "", "", ce
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return "", "", err
		}
	}
	if running && settings["AuthRequired"] == "false" {
		password = ""
	}
	return "ws://127.0.0.1:" + strconv.Itoa(port), password, nil
}

func obsUnitProperty(ctx context.Context, property string) (string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", headlessOBSUnit, "--property="+property, "--value").Output()
	return strings.TrimSpace(string(out)), err
}

func launchHeadlessOBS(ctx context.Context) (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	configHome := filepath.Join(home, ".config", "cs2-headless")
	env, err := obsUnitProperty(ctx, "Environment")
	if err != nil || !strings.Contains(" "+env+" ", " DISPLAY=:20 ") || !strings.Contains(" "+env+" ", " XDG_CONFIG_HOME="+configHome+" ") {
		return "", "", errors.New("未找到使用 DISPLAY=:20 和独立配置目录的 cs2-headless-obs.service，请先安装本机无头会话")
	}
	state, err := obsUnitProperty(ctx, "ActiveState")
	if err != nil {
		return "", "", err
	}
	if state != "active" && state != "inactive" && state != "failed" {
		return "", "", errors.New("无头 OBS 服务正在切换状态，请稍后重试")
	}
	url, password, err := prepareOBSWebsocket(filepath.Join(configHome, "obs-studio", "global.ini"), state == "active")
	if err != nil {
		return "", "", err
	}
	if state != "active" {
		if err := exec.CommandContext(ctx, "systemctl", "--user", "start", headlessOBSUnit).Run(); err != nil {
			return "", "", fmt.Errorf("启动无头 OBS 服务失败，请检查 cs2-headless-obs.service 日志：%w", err)
		}
	}
	var lastErr error
	for {
		pid, err := obsUnitProperty(ctx, "MainPID")
		if err != nil {
			return "", "", err
		}
		if pid == "0" || pid == "" {
			return "", "", errors.New("无头 OBS 已退出，请检查 cs2-headless-obs.service 日志")
		}
		env, err := os.ReadFile(filepath.Join("/proc", pid, "environ"))
		if err != nil {
			return "", "", fmt.Errorf("读取无头 OBS 进程失败：%w", err)
		}
		if !strings.Contains("\x00"+string(env), "\x00DISPLAY=:20\x00") {
			return "", "", errors.New("OBS 进程不在无头显示器 :20，已停止连接配置")
		}
		o, err := openOBS(Config{OBSURL: url, OBSPassword: password})
		if err == nil {
			_, err = o.call("GetVersion", nil)
			o.close()
		}
		if err == nil {
			return url, password, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("等待无头 OBS WebSocket 就绪超时：%w", lastErr)
		case <-time.After(time.Second):
		}
	}
}
