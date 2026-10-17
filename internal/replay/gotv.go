package replay

import (
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var gotvHost = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)
var gotvPasswordCommand = regexp.MustCompile(`(?i)^password\s+(?:"([^"\\;]*)"|([^\s"\\;]+))$`)

// Parse a platform connection string, never arbitrary console commands.
// Password is optional; an explicitly empty quoted password clears it.
func parseGOTV(raw string) (address string, password *string, err error) {
	invalid := errors.New("请输入 host:端口 或 connect host:端口;password 密码（含空格的密码用双引号包裹）")
	raw = strings.TrimSpace(raw)
	if len(raw) > 2048 || strings.ContainsAny(raw, "\r\n") {
		return "", nil, invalid
	}
	raw = strings.TrimSpace(strings.TrimSuffix(raw, ";"))
	parts := strings.Split(raw, ";")
	if len(parts) == 2 && gotvPasswordCommand.MatchString(strings.TrimSpace(parts[0])) {
		parts[0], parts[1] = parts[1], parts[0]
	}
	if len(parts) > 2 {
		return "", nil, invalid
	}
	fields := strings.Fields(parts[0])
	switch {
	case len(fields) == 1:
		address = fields[0]
	case len(fields) == 2 && strings.EqualFold(fields[0], "connect"):
		address = fields[1]
	default:
		return "", nil, invalid
	}
	address = strings.ReplaceAll(address, "：", ":")
	host, port, splitErr := net.SplitHostPort(address)
	n, portErr := strconv.Atoi(port)
	if splitErr != nil || portErr != nil || n < 1 || n > 65535 || (net.ParseIP(host) == nil && (!gotvHost.MatchString(host) || strings.Contains(host, "..") || len(host) > 253)) {
		return "", nil, invalid
	}
	// Canonicalize before comparing addresses for password retention.
	address = net.JoinHostPort(host, strconv.Itoa(n))
	if len(parts) == 2 {
		match := gotvPasswordCommand.FindStringSubmatch(strings.TrimSpace(parts[1]))
		if match == nil {
			return "", nil, invalid
		}
		value := match[1]
		if match[2] != "" {
			value = match[2]
		}
		if err := validateGOTVPassword(value); err != nil {
			return "", nil, err
		}
		password = &value
	}
	return address, password, nil
}
func validateGOTVPassword(password string) error {
	if len(password) > 512 || strings.ContainsAny(password, "\"\\;") || strings.IndexFunc(password, unicode.IsControl) >= 0 {
		return errors.New("GOTV 密码不能超过 512 字节或包含引号、分号、反斜线和控制字符")
	}
	return nil
}
func normalizeGOTV(c *Config, old Config) error {
	if strings.TrimSpace(c.GOTV) == "" {
		c.GOTV = ""
		c.GOTVPassword = ""
		return nil
	}
	address, password, err := parseGOTV(c.GOTV)
	if err != nil {
		return err
	}
	if password != nil {
		c.GOTVPassword = *password
	} else if c.GOTVPassword == "" && address == old.GOTV {
		c.GOTVPassword = old.GOTVPassword
	}
	c.GOTV = address
	return validateGOTVPassword(c.GOTVPassword)
}

type gotvRequest struct {
	Force    bool    `json:"force,omitempty"`
	GOTV     string  `json:"gotv"`
	Password *string `json:"password,omitempty"`
}

func (a *Service) gotvRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/gotv-status", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		c, status := a.s.Config, a.gotvReconnect
		if a.gotvSentAt > 0 && status.Phase == "sent" && nowMS()-a.gotvSentAt > 30000 {
			status = CS2Launch{Phase: "timeout", Message: "连接命令已发送，但 30 秒内未收到比赛 GSI；请检查 B 路游戏连接、密码和 GSI 配置"}
		}
		if status.Phase == "receiving" && nowMS()-a.gsiSeen["b"] > 5000 {
			status = CS2Launch{Phase: "stale", Message: "B 路比赛 GSI 已中断；不能确认 GOTV 仍在线"}
		}
		a.mu.Unlock()
		if a.role == "director" {
			if err := remoteCall(r.Context(), c, "GET", "/api/gotv-status", nil, &status); err != nil {
				fail(w, err)
				return
			}
		}
		respond(w, 200, status)
	})
	api.HandleFunc("POST /api/connect-gotv", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("agent") == "1" && a.role != "agent" {
			fail(w, errors.New("连接目标必须是 Linux Agent"))
			return
		}
		var p gotvRequest
		if err := decode(w, r, &p); err != nil {
			fail(w, err)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		c := a.s.Config
		if !p.Force && (a.mappingBusy || a.gotvReconnect.Phase == "starting" || a.active || a.pendingJobsLocked() || a.cs2Launch.Phase == "starting" || a.localDemo.Phase == "armed" || a.localDemo.Phase == "running" || c.Mode != "live") {
			fail(w, errors.New("请在真实模式下等待录制 / Demo 任务结束后连接 GOTV"))
			return
		}
		if p.GOTV != "" {
			c.GOTV = p.GOTV
			c.GOTVPassword = ""
		}
		if err := normalizeGOTV(&c, a.s.Config); err != nil {
			fail(w, err)
			return
		}
		if c.GOTV == "" {
			fail(w, errors.New("请填写 GOTV 地址或平台连接指令"))
			return
		}
		if p.Password != nil {
			if err := validateGOTVPassword(*p.Password); err != nil {
				fail(w, err)
				return
			}
			c.GOTVPassword = *p.Password
		}
		if a.role == "director" {
			if err := remoteCall(r.Context(), c, "POST", "/api/connect-gotv?agent=1", gotvRequest{GOTV: c.GOTV, Password: &c.GOTVPassword, Force: p.Force}, nil); err != nil {
				fail(w, err)
				return
			}
			a.s.Config.GOTV = c.GOTV
			a.s.Config.GOTVPassword = c.GOTVPassword
			a.gotvManualSendKey = c.RelayPair + "\x00" + c.GOTV + "\x00" + c.GOTVPassword
		} else {
			if p.Force {
				if a.gotvReconnect.Phase == "starting" {
					fail(w, errors.New("B 路正在处理重连，请稍候查看连接状态"))
					return
				}
				a.beginGOTVReconnectLocked(c)
				respond(w, http.StatusAccepted, map[string]string{"message": "B 路已接收重连请求，正在结束旧任务并重新连接", "gotv": c.GOTV})
				return
			}
			release, err := resourceLock(c.SessionLock)
			if err != nil {
				fail(w, err)
				return
			}
			defer release()
			// Set the password BEFORE connecting, regardless of the platform's order.
			// Explicitly clear a previous server password when this address has none.
			command := "spec_show_xray 1; password \"" + c.GOTVPassword + "\"; connect " + c.GOTV
			if err = netCommand(c.NetCon, command); err != nil {
				fail(w, err)
				return
			}
			a.resetDemoTimelineLocked()
			a.s.Config.GOTV = c.GOTV
			a.s.Config.GOTVPassword = c.GOTVPassword
			a.s.Config.Epoch++
			a.s.Config.Paused = !a.s.Config.AutoCapture
			a.gotvReconnect = CS2Launch{Phase: "sent", Message: "GOTV 连接命令已发送，等待比赛 GSI；尚未确认服务器连接成功"}
			a.gotvSentAt = nowMS()
		}
		a.saveLocked()
		if a.storageErr != "" {
			fail(w, errors.New(a.storageErr))
			return
		}
		respond(w, 200, map[string]string{"message": "B 路已接收连接请求，请查看 B 路连接状态或游戏画面", "gotv": c.GOTV})
	})
}
