package replay

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxHUDZip = 100 << 20
const maxHUDExpanded = 512 << 20

type HUDPackage struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Entry  string `json:"entry"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func safeHUDPath(name string) bool {
	return name != "" && name != "." && !strings.ContainsAny(name, "\\:\x00") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}
func unpackHUDZip(archive, dest, name string) (HUDPackage, error) {
	p := HUDPackage{Name: name, Width: 1920, Height: 1080}
	p.ID = id()
	z, err := zip.OpenReader(archive)
	if err != nil {
		return p, errors.New("文件不是有效的 ZIP 压缩包")
	}
	defer z.Close()
	if len(z.File) > 4000 {
		return p, errors.New("HUD 包文件数超过 4000")
	}
	seen := map[string]bool{}
	var total uint64
	for _, f := range z.File {
		n := strings.TrimSuffix(f.Name, "/")
		if !safeHUDPath(n) || f.Mode()&os.ModeSymlink != 0 || !f.FileInfo().IsDir() && !f.Mode().IsRegular() {
			return p, errors.New("HUD ZIP 含不安全路径或特殊文件")
		}
		if seen[n] {
			return p, errors.New("HUD ZIP 含重复路径")
		}
		seen[n] = true
		if f.UncompressedSize64 > 64<<20 || f.UncompressedSize64 > maxHUDExpanded-total {
			return p, errors.New("HUD 解压尺寸超过限制（单文件 64 MB、总计 512 MB）")
		}
		total += f.UncompressedSize64
	}
	if err := os.MkdirAll(dest, 0700); err != nil {
		return p, err
	}
	indexes := []string{}
	for _, f := range z.File {
		n := strings.TrimSuffix(f.Name, "/")
		if strings.HasPrefix(n, "__MACOSX/") || path.Base(n) == ".DS_Store" {
			continue
		}
		out := filepath.Join(dest, filepath.FromSlash(n))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(out, 0700); err != nil {
				return p, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
			return p, err
		}
		r, err := f.Open()
		if err != nil {
			return p, err
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			r.Close()
			return p, err
		}
		count, e := io.Copy(w, io.LimitReader(r, int64(f.UncompressedSize64)+1))
		r.Close()
		ce := w.Close()
		if e != nil {
			return p, e
		}
		if ce != nil {
			return p, ce
		}
		if uint64(count) != f.UncompressedSize64 {
			return p, errors.New("ZIP 文件长度不符")
		}
		if strings.EqualFold(path.Base(n), "index.html") {
			indexes = append(indexes, n)
		}
	}
	// A manifest is optional; relative entry paths are resolved from its directory.
	manifest := ""
	if seen["replay-hud.json"] {
		manifest = "replay-hud.json"
	} else {
		for n := range seen {
			if path.Base(n) == "replay-hud.json" {
				if manifest != "" {
					return p, errors.New("发现多个 replay-hud.json，请仅保留一个入口配置")
				}
				manifest = n
			}
		}
	}
	if manifest != "" {
		data, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(manifest)))
		if err != nil {
			return p, err
		}
		var m struct {
			Name   string `json:"name"`
			Entry  string `json:"entry"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		}
		if len(data) > 64<<10 || json.Unmarshal(data, &m) != nil {
			return p, errors.New("replay-hud.json 格式无效")
		}
		if !safeHUDPath(m.Entry) {
			return p, errors.New("manifest entry 必须是包内相对 HTML 路径")
		}
		p.Entry = path.Join(path.Dir(manifest), m.Entry)
		if m.Name != "" {
			p.Name = m.Name
		}
		if m.Width != 0 {
			p.Width = m.Width
		}
		if m.Height != 0 {
			p.Height = m.Height
		}
	} else if seen["index.html"] {
		p.Entry = "index.html"
	} else if len(indexes) == 1 {
		p.Entry = indexes[0]
	} else {
		return p, errors.New("未找到唯一 index.html；请在包内添加 replay-hud.json 指定 entry")
	}
	if len(p.Name) > 200 {
		p.Name = string([]rune(p.Name)[:min(80, len([]rune(p.Name)))])
	}
	if !strings.HasSuffix(strings.ToLower(p.Entry), ".html") {
		return p, errors.New("HUD 入口必须是 HTML 文件")
	}
	if st, err := os.Stat(filepath.Join(dest, filepath.FromSlash(p.Entry))); err != nil || !st.Mode().IsRegular() || st.Size() > 8<<20 {
		return p, errors.New("HUD 入口文件不存在或超过 8 MB")
	}
	if err := (HUDSettings{Width: p.Width, Height: p.Height}).validate(); err != nil {
		return p, err
	}
	return p, nil
}
func (a *Service) hudZipRoutes(mux, api *http.ServeMux) {
	api.HandleFunc("POST /api/hud/import", a.importHUDZip)
	mux.HandleFunc("GET /hud-packages/{id}/{asset...}", a.serveHUDPackage)
	mux.HandleFunc("GET /hud-data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		a.mu.Lock()
		defer a.mu.Unlock()
		g := jsonCopy(a.previous["b"])
		delete(g, "auth")
		respond(w, 200, map[string]any{"gsi": g, "teams": a.s.Config.Teams, "visible": !a.hudHidden, "fresh": nowMS()-a.gsiSeen["b"] < 2000})
	})
}
func (a *Service) serveHUDPackage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	asset := r.PathValue("asset")
	if !validID(id) || !safeHUDPath(asset) {
		http.NotFound(w, r)
		return
	}
	// Imported HTML has an opaque sandbox origin and cannot call the control API.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts; default-src 'self' data: blob: http: https:; script-src 'self' 'unsafe-inline' 'unsafe-eval' http: https:; style-src 'self' 'unsafe-inline' http: https:; connect-src 'self' http: https: ws: wss:; object-src 'none'")
	if asset == "__replay_bridge.js" {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		io.WriteString(w, hudBridge)
		return
	}
	root := filepath.Join(a.dir, "hud-packages", id)
	full := filepath.Join(root, filepath.FromSlash(asset))
	st, err := os.Stat(full)
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(strings.ToLower(asset), ".html") {
		if st.Size() > 8<<20 {
			http.Error(w, "HUD HTML 超过 8 MB", 400)
			return
		}
		b, err := os.ReadFile(full)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		bridge := "<script src=\"/hud-packages/" + id + "/__replay_bridge.js\"></script>"
		html := string(b)
		lower := strings.ToLower(html)
		if i := strings.Index(lower, "<head"); i >= 0 {
			if j := strings.Index(html[i:], ">"); j >= 0 {
				i += j + 1
				html = html[:i] + bridge + html[i:]
			}
		} else {
			html = bridge + html
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, html)
		return
	}
	http.ServeFile(w, r, full)
}

const hudBridge = `(()=>{const root=new URL(document.currentScript.src).origin;window.ReplayHUD={state:null};async function update(){try{const r=await fetch(root+'/hud-data',{credentials:'omit',cache:'no-store'});if(!r.ok)throw Error(r.status);const s=await r.json();window.ReplayHUD.state=s;window.dispatchEvent(new CustomEvent('replay-hud',{detail:s}));window.dispatchEvent(new CustomEvent('gsi',{detail:s.gsi}));}catch{}setTimeout(update,100)}update()})();`

func (a *Service) importHUDZip(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if runtime.GOOS != "linux" || a.role != "agent" || a.active || a.pendingJobsLocked() || a.localDemo.Phase == "armed" || a.localDemo.Phase == "running" || a.obsLaunch.Phase == "starting" || a.cs2Launch.Phase == "starting" || a.hudImport.Phase == "starting" {
		a.mu.Unlock()
		fail(w, errors.New("请在 Linux 空闲时导入 HUD"))
		return
	}
	lock := a.s.Config.SessionLock
	if lock == "" {
		lock = filepath.Join(a.dir, "hud-import.lock")
	}
	release, err := resourceLock(lock)
	if err != nil {
		a.mu.Unlock()
		fail(w, err)
		return
	}
	a.hudImport = CS2Launch{"starting", "正在接收 HUD ZIP…"}
	a.mu.Unlock()
	failed := func(err error) {
		release()
		a.mu.Lock()
		a.hudImport = CS2Launch{"failed", err.Error()}
		a.mu.Unlock()
		fail(w, err)
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
	tmp, err := os.CreateTemp(a.dir, "hud-upload-*.zip")
	if err != nil {
		failed(err)
		return
	}
	_, err = io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxHUDZip))
	ce := tmp.Close()
	if err != nil || ce != nil {
		os.Remove(tmp.Name())
		if err == nil {
			err = ce
		}
		failed(fmt.Errorf("上传失败（ZIP 最大 100 MB）：%w", err))
		return
	}
	name := path.Base(strings.ReplaceAll(r.URL.Query().Get("name"), "\\", "/"))
	if name == "." || name == "/" || name == "" {
		name = "自定义 HUD"
	}
	a.mu.Lock()
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		defer release()
		defer os.Remove(tmp.Name())
		p, err := a.activateHUDZip(tmp.Name(), name, r.Host)
		a.mu.Lock()
		defer a.mu.Unlock()
		if err != nil {
			a.hudImport = CS2Launch{"failed", err.Error()}
			a.logLocked("warn", "HUD 导入失败："+err.Error())
		} else {
			a.hudImport = CS2Launch{"ready", "已导入并启用 HUD：" + p.Name}
			a.logLocked("info", a.hudImport.Message)
		}
	}()
	respond(w, http.StatusAccepted, map[string]bool{"ok": true})
}
func (a *Service) activateHUDZip(archive, name, host string) (HUDPackage, error) {
	return a.activateHUDZipWithOBS(archive, name, host, launchHeadlessOBS)
}
func (a *Service) activateHUDZipWithOBS(archive, name, host string, launch func(context.Context) (string, string, error)) (HUDPackage, error) {
	p := HUDPackage{}
	base := filepath.Join(a.dir, "hud-packages")
	if err := os.MkdirAll(base, 0700); err != nil {
		return p, err
	}
	stage, err := os.MkdirTemp(base, ".import-")
	if err != nil {
		return p, err
	}
	defer os.RemoveAll(stage)
	p, err = unpackHUDZip(archive, stage, name)
	if err != nil {
		return p, err
	}
	dest := filepath.Join(base, p.ID)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		if err = os.Rename(stage, dest); err != nil {
			return p, err
		}
	} else if err != nil {
		return p, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	defer cancel()
	obsURL, password, err := launch(ctx)
	if err != nil {
		return p, err
	}
	a.mu.Lock()
	c := a.s.Config
	previous := a.s.HUDActiveSource
	oldHUD := a.s.HUD
	oldTeam := a.s.TeamHUD
	oldPackage := a.s.HUDPackage
	a.mu.Unlock()
	c.OBSURL, c.OBSPassword = obsURL, password
	o, err := openOBS(c)
	if err != nil {
		return p, err
	}
	defer o.close()
	h, err := packageHUD(p, host)
	if err != nil {
		return p, err
	}
	if previous == "" && oldTeam {
		previous = "Project Replay Team HUD"
	}

	for _, kind := range []string{"GetRecordStatus", "GetStreamStatus"} {
		state, e := o.call(kind, nil)
		if e != nil {
			return p, e
		}
		if busy, _ := state["outputActive"].(bool); busy {
			return p, errors.New("OBS 正在录制或直播，HUD 未切换")
		}
	}
	scene, e := o.call("GetCurrentProgramScene", nil)
	if e != nil {
		return p, e
	}
	sceneName := stringField(scene, "currentProgramSceneName")
	before, e := o.call("GetSceneItemList", map[string]any{"sceneName": sceneName})
	if e != nil {
		return p, e
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rows, _ := before["sceneItems"].([]any)
		for _, row := range rows {
			m, _ := row.(map[string]any)
			if enabled, ok := m["sceneItemEnabled"].(bool); ok {
				_, _ = o.call("SetSceneItemEnabled", map[string]any{"sceneName": sceneName, "sceneItemId": m["sceneItemId"], "sceneItemEnabled": enabled})
			}
		}
		_, _ = o.call("RemoveInput", map[string]any{"inputName": h.Source})
	}()
	_, input, err := installHUDSource(o, h, host, previous)
	if err != nil {
		return p, fmt.Errorf("ZIP 已解压，但 OBS 启用失败，已尝试恢复原 HUD：%w", err)
	}
	a.mu.Lock()
	a.s.HUD = h
	a.s.HUDPackage = p
	a.s.HUDActiveSource = input
	a.s.TeamHUD = true
	a.hudHidden = false
	a.s.Config.OBSURL = obsURL
	a.s.Config.OBSPassword = password
	a.saveLocked()
	saveErr := a.storageErr
	if saveErr != "" {
		a.s.HUD = oldHUD
		a.s.HUDPackage = oldPackage
		a.s.HUDActiveSource = previous
		a.s.TeamHUD = oldTeam
	}
	a.mu.Unlock()
	if saveErr != "" {
		return p, errors.New("保存 HUD 配置失败：" + saveErr)
	}
	committed = true
	if c.Mode == "live" {
		if err := hideGameUI(c.NetCon); err != nil {
			a.mu.Lock()
			a.logLocked("warn", "ZIP HUD 已启用；游戏未就绪，启动 CS2 后将应用原生 HUD 隐藏设置")
			a.mu.Unlock()
		}
	}
	return p, nil
}

func packageHUD(p HUDPackage, host string) (HUDSettings, error) {
	_, port, err := net.SplitHostPort(host)
	if err != nil {
		return HUDSettings{}, err
	}
	entry := (&url.URL{Path: "/hud-packages/" + p.ID + "/" + p.Entry}).EscapedPath()
	return HUDSettings{Mode: "package", URL: "http://127.0.0.1:" + port + entry, Source: "Project Replay ZIP HUD " + p.ID, Width: p.Width, Height: p.Height}, nil
}
