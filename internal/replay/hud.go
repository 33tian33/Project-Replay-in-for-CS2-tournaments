package replay

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const fullHUDCommand = "gameui_hide; hideconsole; demo_ui_mode 0; cl_drawhud 1; crosshair 1; cl_draw_only_deathnotices 0; cl_drawhud_force_radar 0; cl_drawhud_force_deathnotices 0; cl_drawhud_force_teamid_overhead 0; spec_show_xray 1"

// Match CSStudio config/openhud_headless.json.
const teamHUDCommand = "sv_cheats 1; gameui_hide; hideconsole; cl_drawhud 0; crosshair 0; demo_ui_mode 0; cl_draw_only_deathnotices 0; cl_drawhud_force_deathnotices -1; cl_drawhud_force_radar -1; cl_drawhud_force_teamid_overhead -1; spec_show_xray 1"

func (a *Service) recordingHUDCommandLocked() string {
	if a.s.TeamHUD || a.s.HUDActiveSource != "" || a.hudHidden {
		return teamHUDCommand
	}
	return fullHUDCommand
}
func (a *Service) recordingHUDCommand() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recordingHUDCommandLocked()
}
func showGameUI(address string) error {
	n, err := openConsole(address)
	if err != nil {
		return err
	}
	defer n.conn.Close()
	if _, err = n.queryWithTimeout(fullHUDCommand, time.Second); err != nil {
		return err
	}
	reply, err := n.queryWithTimeout("cl_drawhud", time.Second)
	if err != nil {
		return err
	}
	if visible, known := hudValue(reply, "cl_drawhud"); !known || !visible {
		return errors.New("游戏尚未确认显示原生 HUD")
	}
	return nil
}

func hideGameUI(address string) error {
	n, err := openConsole(address)
	if err != nil {
		return err
	}
	defer n.conn.Close()
	if _, err = n.queryWithTimeout(teamHUDCommand, time.Second); err != nil {
		return err
	}
	reply, err := n.queryWithTimeout("cl_drawhud; crosshair; cl_draw_only_deathnotices; cl_drawhud_force_deathnotices; cl_drawhud_force_radar; cl_drawhud_force_teamid_overhead; spec_show_xray", time.Second)
	if err != nil {
		return err
	}
	for _, name := range []string{"cl_drawhud", "crosshair", "cl_draw_only_deathnotices"} {
		if value, known := hudValue(reply, name); !known || value {
			return fmt.Errorf("游戏未确认关闭 %s：%.800s", name, reply)
		}
	}
	if value, known := hudValue(reply, "spec_show_xray"); !known || !value {
		return fmt.Errorf("游戏未确认开启 X 光：%.800s", reply)
	}
	for _, name := range []string{"cl_drawhud_force_deathnotices", "cl_drawhud_force_radar", "cl_drawhud_force_teamid_overhead"} {
		re := regexp.MustCompile(`(?mi)"?` + name + `"?\s*(?:=\s*)?"?-1\b`)
		if !re.MatchString(reply) {
			return fmt.Errorf("游戏未确认关闭 %s：%.800s", name, reply)
		}
	}
	return nil
}

func hudValue(reply, name string) (bool, bool) {
	re := regexp.MustCompile(`(?mi)["\x20]?` + regexp.QuoteMeta(name) + `["\x20]*\s*(?:=\s*)?["\x20]*(true|false|0|1)\b`)
	m := re.FindStringSubmatch(reply)
	if m == nil {
		return false, false
	}
	return m[1] == "1" || strings.EqualFold(m[1], "true"), true
}
func (a *Service) setHUD(visible bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if visible && a.s.HUDActiveSource != "" {
		return errors.New("OBS HUD 已启用，不能叠加游戏原生 HUD")
	}
	for _, j := range a.s.Jobs {
		if reservesCamera(j.Status) {
			return errors.New("录制任务已锁定，请等待完成后切换 HUD")
		}
	}
	if a.active && !a.pendingJobsLocked() || a.localDemo.Phase == "armed" {
		return errors.New("录制任务已锁定，请等待完成后切换 HUD")
	}
	c := a.s.Config
	if a.role != "agent" || c.Mode != "live" {
		return errors.New("请在 Linux Agent 的真实模式切换 HUD")
	}
	lock := c.SessionLock
	if lock == "" {
		lock = filepath.Join(a.dir, "hud.lock")
	}
	release, err := resourceLock(lock)
	if err != nil {
		return err
	}
	defer release()
	n, err := openConsole(c.NetCon)
	if err != nil {
		return err
	}
	defer n.conn.Close()
	command := teamHUDCommand
	if visible {
		command = fullHUDCommand
	}
	if _, err = n.queryWithTimeout(command, time.Second); err != nil {
		return fmt.Errorf("HUD 命令未收到游戏回执：%w", err)
	}
	reply, err := n.queryWithTimeout("cl_drawhud; cl_draw_only_deathnotices", time.Second)
	if err != nil {
		return err
	}
	value, known := hudValue(reply, "cl_drawhud")
	deathOnly, deathKnown := hudValue(reply, "cl_draw_only_deathnotices")
	if !known || value != visible || visible && (!deathKnown || deathOnly) {
		return fmt.Errorf("游戏未确认 HUD 状态；cl_drawhud 可能受命令权限限制。请检查 NetCon / Demo 权限，或使用“安装战队 HUD 到 OBS”。反馈：%.800s", reply)
	}
	a.s.TeamHUD = false
	a.hudHidden = !visible
	a.logLocked("info", "已回读确认原生 HUD 状态；战队图标通过 OBS 战队 HUD 显示")
	a.saveLocked()
	return nil
}
func (a *Service) hudRoutes(api *http.ServeMux) {
	api.HandleFunc("POST /api/hud/obs", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.role != "agent" || a.active || a.pendingJobsLocked() || a.obsLaunch.Phase == "starting" || a.localDemo.Phase == "armed" || a.localDemo.Phase == "running" || a.cs2Launch.Phase == "starting" {
			fail(w, errors.New("请在 Linux 空闲时安装 HUD"))
			return
		}
		lock := a.s.Config.SessionLock
		if lock == "" {
			lock = filepath.Join(a.dir, "hud.lock")
		}
		release, err := resourceLock(lock)
		if err != nil {
			fail(w, err)
			return
		}
		defer release()
		o, err := openOBS(a.s.Config)
		if err != nil {
			fail(w, err)
			return
		}
		defer o.close()
		previous := a.s.HUDActiveSource
		if previous == "" && a.s.TeamHUD {
			previous = "Project Replay Team HUD"
		}
		sceneName, input, err := installHUDSource(o, a.s.HUD, r.Host, previous)
		if err != nil {
			fail(w, err)
			return
		}
		a.s.HUDActiveSource = input
		a.s.HUD = a.s.HUD.normalized()
		a.s.TeamHUD = true
		a.hudHidden = false
		warning := ""
		if a.s.Config.Mode == "live" {
			var gameErr error
			if a.s.TeamHUD {
				gameErr = hideGameUI(a.s.Config.NetCon)
			} else {
				gameErr = showGameUI(a.s.Config.NetCon)
			}
			if gameErr != nil {
				warning = "HUD 已接入 OBS；游戏原生 HUD 状态尚未同步，启动 CS2 后请再次应用 HUD：" + gameErr.Error()
			}
		}
		a.saveLocked()
		if a.storageErr != "" {
			fail(w, errors.New(a.storageErr))
			return
		}
		a.logLocked("info", "已接入 HUD 源："+input)
		respond(w, 200, map[string]any{"ok": true, "scene": sceneName, "source": input, "warning": warning})
	})
}
