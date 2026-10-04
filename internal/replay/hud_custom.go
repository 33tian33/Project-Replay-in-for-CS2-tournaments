package replay

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type HUDSettings struct {
	Mode       string `json:"mode"`
	URL        string `json:"url"`
	Source     string `json:"source"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	KeepNative bool   `json:"keep_native"`
}

func (h HUDSettings) normalized() HUDSettings {
	if h.Mode == "" {
		h.Mode = "builtin"
	}
	if h.Width == 0 {
		h.Width = 1920
	}
	if h.Height == 0 {
		h.Height = 1080
	}
	h.URL = strings.TrimSpace(h.URL)
	h.Source = strings.TrimSpace(h.Source)
	return h
}
func (h HUDSettings) validate() error {
	h = h.normalized()
	if h.Width < 64 || h.Width > 7680 || h.Height < 64 || h.Height > 4320 {
		return errors.New("HUD 尺寸须在 64–7680 × 64–4320 之间")
	}
	switch h.Mode {
	case "builtin":
	case "package":
		u, e := url.Parse(h.URL)
		if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || !strings.HasPrefix(u.Path, "/hud-packages/") || !strings.HasPrefix(h.Source, "Project Replay ZIP HUD ") {
			return errors.New("ZIP HUD 配置无效，请重新导入")
		}
	case "url":
		u, e := url.Parse(h.URL)
		if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || len(h.URL) > 4096 || strings.ContainsAny(h.URL, "\r\n\x00") {
			return errors.New("请输入 HUD 程序提供的 http(s) 网页输出地址")
		}
	case "source":
		if h.Source == "" || len(h.Source) > 200 || strings.ContainsAny(h.Source, "\r\n\x00") || h.Source == "Project Replay Team HUD" || h.Source == "Project Replay Custom HUD" {
			return errors.New("请选择独立 HUD 的 OBS 源，不能使用 Replay 自有 HUD 源")
		}
	default:
		return errors.New("HUD 类型须为内置、程序网页输出或已有 OBS 源")
	}
	return nil
}
func (a *Service) hudSettingsRoutes(api *http.ServeMux) {
	api.HandleFunc("POST /api/hud/settings", func(w http.ResponseWriter, r *http.Request) {
		var h HUDSettings
		if err := decode(w, r, &h); err != nil {
			fail(w, err)
			return
		}
		h = h.normalized()
		if h.Mode == "package" {
			a.mu.Lock()
			p := a.s.HUDPackage
			a.mu.Unlock()
			if !validID(p.ID) {
				fail(w, errors.New("请先导入 HUD ZIP"))
				return
			}
			native := h.KeepNative
			var e error
			h, e = packageHUD(p, r.Host)
			if e != nil {
				fail(w, e)
				return
			}
			h.KeepNative = native
		}
		if err := h.validate(); err != nil {
			fail(w, err)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.role != "agent" || a.active || a.pendingJobsLocked() || a.localDemo.Phase == "armed" || a.localDemo.Phase == "running" || a.cs2Launch.Phase == "starting" || a.obsLaunch.Phase == "starting" {
			fail(w, errors.New("请在 Linux 空闲时设置 HUD"))
			return
		}
		a.s.HUD = h
		a.saveLocked()
		if a.storageErr != "" {
			fail(w, errors.New(a.storageErr))
			return
		}
		respond(w, 200, map[string]any{"ok": true, "hud": h})
	})
	api.HandleFunc("GET /api/hud/inputs", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		c, role := a.s.Config, a.role
		a.mu.Unlock()
		if role != "agent" {
			fail(w, errors.New("请在 Linux 录制端读取 OBS 源"))
			return
		}
		o, err := openOBS(c)
		if err != nil {
			fail(w, err)
			return
		}
		defer o.close()
		list, err := o.call("GetInputList", nil)
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, 200, list)
	})
}

func installHUDSource(o *obs, h HUDSettings, host, previous string) (string, string, error) {
	h = h.normalized()
	if err := h.validate(); err != nil {
		return "", "", err
	}
	for _, kind := range []string{"GetRecordStatus", "GetStreamStatus"} {
		st, err := o.call(kind, nil)
		if err != nil {
			return "", "", err
		}
		if active, _ := st["outputActive"].(bool); active {
			return "", "", errors.New("OBS 正在录制或直播，请先停止后再切换 HUD")
		}
	}
	scene, err := o.call("GetCurrentProgramScene", nil)
	if err != nil {
		return "", "", err
	}
	sceneName := stringField(scene, "currentProgramSceneName")
	input := "Project Replay Team HUD"
	settings := map[string]any{"width": h.Width, "height": h.Height, "is_local_file": false, "shutdown": false, "restart_when_active": false, "css": "body { background-color: rgba(0,0,0,0); margin: 0; overflow: hidden; }"}
	switch h.Mode {
	case "builtin":
		_, port, e := net.SplitHostPort(host)
		if e != nil {
			return "", "", e
		}
		settings["url"] = "http://127.0.0.1:" + port + "/hud.html"
	case "package":
		input = h.Source
		settings["url"] = h.URL
	case "url":
		input = "Project Replay Custom HUD"
		settings["url"] = h.URL
	case "source":
		input = h.Source
	}
	inputs, err := o.call("GetInputList", nil)
	if err != nil {
		return "", "", err
	}
	exists := false
	list, _ := inputs["inputs"].([]any)
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		if stringField(m, "inputName") == input {
			exists = true
			if h.Mode != "source" && stringField(m, "inputKind") != "" && stringField(m, "inputKind") != "browser_source" {
				return "", "", fmt.Errorf("OBS 源 %s 不是浏览器源", input)
			}
		}
	}
	if h.Mode == "source" && !exists {
		return "", "", errors.New("未找到所选 OBS 源，请先在无头 OBS 中添加 HUD 浏览器源或窗口采集源")
	}
	var item map[string]any
	if !exists {
		item, err = o.call("CreateInput", map[string]any{"sceneName": sceneName, "inputName": input, "inputKind": "browser_source", "inputSettings": settings, "sceneItemEnabled": true})
	} else {
		if h.Mode != "source" {
			_, err = o.call("SetInputSettings", map[string]any{"inputName": input, "inputSettings": settings, "overlay": true})
			if err != nil {
				return "", "", err
			}
		}
		item, err = o.call("GetSceneItemId", map[string]any{"sceneName": sceneName, "sourceName": input})
		if err != nil {
			item, err = o.call("CreateSceneItem", map[string]any{"sceneName": sceneName, "sourceName": input, "sceneItemEnabled": true})
		}
	}
	if err != nil {
		return "", "", err
	}
	itemID := number(item, "sceneItemId")
	if _, err = o.call("SetSceneItemEnabled", map[string]any{"sceneName": sceneName, "sceneItemId": itemID, "sceneItemEnabled": true}); err != nil {
		return "", "", err
	}
	items, err := o.call("GetSceneItemList", map[string]any{"sceneName": sceneName})
	if err != nil {
		return "", "", err
	}
	rows, _ := items["sceneItems"].([]any)
	if _, err = o.call("SetSceneItemIndex", map[string]any{"sceneName": sceneName, "sceneItemId": itemID, "sceneItemIndex": max(0, len(rows)-1)}); err != nil {
		return "", "", err
	}
	if h.Mode != "source" {
		video, e := o.call("GetVideoSettings", nil)
		if e != nil {
			return "", "", e
		}
		_, err = o.call("SetSceneItemTransform", map[string]any{"sceneName": sceneName, "sceneItemId": itemID, "sceneItemTransform": map[string]any{"positionX": 0, "positionY": 0, "alignment": 5, "rotation": 0, "cropLeft": 0, "cropRight": 0, "cropTop": 0, "cropBottom": 0, "boundsType": "OBS_BOUNDS_NONE", "scaleX": number(video, "baseWidth") / float64(h.Width), "scaleY": number(video, "baseHeight") / float64(h.Height)}})
		if err != nil {
			return "", "", err
		}
	}
	// Disable only our managed overlays and the previously selected external source.
	for _, raw := range rows {
		m, _ := raw.(map[string]any)
		name := stringField(m, "sourceName")
		if name != input && (name == "Project Replay Team HUD" || name == "Project Replay Custom HUD" || previous != "" && name == previous) {
			if _, err = o.call("SetSceneItemEnabled", map[string]any{"sceneName": sceneName, "sceneItemId": m["sceneItemId"], "sceneItemEnabled": false}); err != nil {
				return "", "", err
			}
		}
	}
	return sceneName, input, nil
}
