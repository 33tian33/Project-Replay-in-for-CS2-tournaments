package replay

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

//go:embed web/*
var webFiles embed.FS

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, e error) { respond(w, 400, map[string]string{"error": e.Error()}) }
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func (a *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFiles, "web")
	static := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != "GET" && r.Method != "HEAD") || r.URL.Path != "/" && r.URL.Path != "/app.js" && r.URL.Path != "/style.css" && r.URL.Path != "/output.html" && r.URL.Path != "/output.js" && r.URL.Path != "/output.css" && r.URL.Path != "/hud.html" && r.URL.Path != "/hud.js" && r.URL.Path != "/hud.css" {
			http.NotFound(w, r)
			return
		}
		static.ServeHTTP(w, r)
	})
	api := http.NewServeMux()
	a.outputRoutes(mux, api)
	a.demoRoutes(api)
	a.relayRoutes(api)
	a.roundRoutes(api)
	a.cs2Routes(api)
	a.teamRoutes(mux, api)
	a.cleanupRoutes(api)
	a.hudRoutes(api)
	a.hudSettingsRoutes(api)
	a.hudZipRoutes(mux, api)
	a.obsLaunchRoutes(api)
	a.timingRoutes(api)
	api.HandleFunc("POST /api/hud", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Visible *bool `json:"visible"`
		}
		if err := decode(w, r, &p); err != nil {
			fail(w, err)
			return
		}
		if p.Visible == nil {
			fail(w, errors.New("必须提供 visible"))
			return
		}
		if err := a.setHUD(*p.Visible); err != nil {
			fail(w, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, a.snapshot()) })
	api.HandleFunc("GET /api/time", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]int64{"now": nowMS()}) })
	api.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		var c Config
		if e := decode(w, r, &c); e != nil {
			fail(w, e)
			return
		}
		if e := a.configure(c); e != nil {
			fail(w, e)
			return
		}
		a.mu.Lock()
		c = jsonCopy(a.s.Config)
		a.mu.Unlock()
		if a.role == "director" && c.AutoSendGOTV && c.Mode == "live" && c.ConnectionMode != "relay" {
			if e := remoteCall(r.Context(), c, "POST", "/api/connect-gotv?agent=1", gotvRequest{GOTV: c.GOTV, Password: &c.GOTVPassword}, nil); e != nil {
				fail(w, fmt.Errorf("本机配置已保存，但 GOTV2 自动发送失败：%w", e))
				return
			}
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/auto-capture", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decode(w, r, &p); err != nil {
			fail(w, err)
			return
		}
		if p.Enabled == nil {
			fail(w, errors.New("必须提供 enabled"))
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.s.Config.AutoCapture = *p.Enabled
		a.s.Config.Paused = !*p.Enabled || a.localDemo.Phase == "armed" || a.cs2Launch.Phase == "starting"
		if *p.Enabled {
			a.s.Config.Strict = false
		}
		a.logLocked("info", fmt.Sprintf("自动录制/发送任务：%t；回合时钟采集无需手工校准", *p.Enabled))
		a.saveLocked()
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Paused bool `json:"paused"`
		}
		if e := decode(w, r, &p); e != nil {
			fail(w, e)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if !p.Paused && a.localDemo.Phase == "armed" {
			fail(w, errors.New("等待本机 Demo 开始播放后再启用采集"))
			return
		}
		a.s.Config.Paused = p.Paused
		a.s.Config.AutoCapture = !p.Paused
		a.saveLocked()
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/calibrate", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Delta       float64 `json:"delta"`
			Uncertainty float64 `json:"uncertainty"`
		}
		if e := decode(w, r, &p); e != nil {
			fail(w, e)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.active {
			fail(w, errors.New("请等待当前任务完成"))
			return
		}
		c := a.s.Config
		c.Delta = p.Delta
		c.Uncertainty = p.Uncertainty
		if e := c.validate(); e != nil {
			fail(w, e)
			return
		}
		if c.Uncertainty > c.Guard {
			fail(w, errors.New("校准误差超过保护量"))
			return
		}
		c.CalibratedUntil = nowMS() + 600000
		if c.AutoCapture && a.localDemo.Phase != "armed" && a.cs2Launch.Phase != "starting" {
			c.Paused = false
		}
		a.s.Config = c
		a.logLocked("info", "已录入人工实测时间差，有效期 10 分钟；未修改 GOTV 服务器延迟")
		a.saveLocked()
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/events", func(w http.ResponseWriter, r *http.Request) {
		var e Event
		if err := decode(w, r, &e); err != nil {
			fail(w, err)
			return
		}
		if err := a.upsert(e); err != nil {
			fail(w, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/pin", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			ID  string `json:"id"`
			Pin bool   `json:"pin"`
		}
		if e := decode(w, r, &p); e != nil {
			fail(w, e)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		for i := range a.s.Events {
			e := &a.s.Events[i]
			if e.ID == p.ID {
				if e.JobID != "" {
					fail(w, errors.New("已提交任务不可更改"))
					return
				}
				e.Pin = p.Pin
				a.saveLocked()
				respond(w, 200, map[string]bool{"ok": true})
				return
			}
		}
		http.NotFound(w, r)
	})
	api.HandleFunc("POST /api/demo", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.s.Config.Mode != "demo" {
			fail(w, errors.New("仅在演示模式使用示例事件"))
			return
		}
		t := nowMS() + 9000
		for i, s := range []string{"76561198000000001", "76561198000000002", "76561198000000002"} {
			name := "donk"
			if i > 0 {
				name = "m0NESY"
			}
			delay := int64(0)
			if i == 1 {
				delay = 200
			}
			if i == 2 {
				delay = 700
			}
			if e := a.upsertLocked(Event{ID: id(), Player: s, Name: name, Time: t + delay, Quality: "demo", Round: 7, Kills: max(i, 1), Uncertainty: 0, GroupCount: 1}); e != nil {
				fail(w, e)
				return
			}
		}
		a.s.Config.Paused = false
		a.saveLocked()
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/preflight", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, a.preflight()) })
	api.HandleFunc("POST /api/remote-sync", func(w http.ResponseWriter, r *http.Request) {
		if e := a.syncRemote(); e != nil {
			fail(w, e)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	a.gotvRoutes(api)
	api.HandleFunc("POST /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		var j Job
		if e := decode(w, r, &j); e != nil {
			fail(w, e)
			return
		}
		result, e := a.acceptJob(j)
		if e != nil {
			fail(w, e)
			return
		}
		respond(w, 200, result)
	})
	api.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, j := range a.s.Jobs {
			if j.ID == r.PathValue("id") {
				respond(w, 200, j)
				return
			}
		}
		http.NotFound(w, r)
	})
	api.HandleFunc("GET /api/media/{id}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		var art *Artifact
		for _, v := range a.s.Artifacts {
			if v.ID == r.PathValue("id") {
				copy := v
				art = &copy
			}
		}
		a.mu.Unlock()
		if art == nil {
			http.NotFound(w, r)
			return
		}
		f, e := os.Open(art.Path)
		if e != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			fail(w, e)
			return
		}
		w.Header().Set("ETag", `"`+art.SHA256+`"`)
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, art.ID+".mp4", st.ModTime(), f)
	})
	api.HandleFunc("GET /api/gsi-config/{side}", func(w http.ResponseWriter, r *http.Request) {
		side := r.PathValue("side")
		if side != "a" && side != "b" {
			http.NotFound(w, r)
			return
		}
		host, port, e := net.SplitHostPort(r.Host)
		if e != nil {
			fail(w, e)
			return
		}
		_ = host
		base := "http://127.0.0.1:" + port
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="gamestate_integration_replay_%s.cfg"`, side))
		fmt.Fprint(w, a.gsiConfig(side, base))
	})
	api.HandleFunc("POST /api/queue", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			IDs []string `json:"ids"`
		}
		if e := decode(w, r, &p); e != nil {
			fail(w, e)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		available := map[string]bool{}
		for _, v := range a.s.Artifacts {
			available[v.ID] = true
		}
		seen := map[string]bool{}
		for _, v := range p.IDs {
			if !available[v] || seen[v] {
				fail(w, errors.New("队列含未知或重复素材"))
				return
			}
			seen[v] = true
		}
		a.s.Queue = p.IDs
		a.saveLocked()
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/play", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			ID string `json:"id"`
		}
		if e := decode(w, r, &p); e != nil {
			fail(w, e)
			return
		}
		if e := a.play(p.ID); e != nil {
			fail(w, e)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/return-live", func(w http.ResponseWriter, r *http.Request) {
		if e := a.returnLive(); e != nil {
			fail(w, e)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	mux.Handle("/api/", api)
	mux.HandleFunc("POST /gsi/{side}", func(w http.ResponseWriter, r *http.Request) {
		side := r.PathValue("side")
		if side != "a" && side != "b" {
			http.NotFound(w, r)
			return
		}
		var p map[string]any
		if e := decode(w, r, &p); e != nil {
			fail(w, e)
			return
		}
		if e := a.ingestGSI(side, p); e != nil {
			fail(w, e)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		hudRead := (r.Method == "GET" || r.Method == "HEAD") && (strings.HasPrefix(r.URL.Path, "/hud-packages/") || r.URL.Path == "/hud-data" || r.URL.Path == "/hud-api/state")
		if origin := r.Header.Get("Origin"); origin != "" && !hudRead {
			u, e := url.Parse(origin)
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			if e != nil || u.Host != r.Host || u.Scheme != scheme {
				respond(w, 403, map[string]string{"error": "禁止跨站请求"})
				return
			}
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.URL.Path != "/api/cleanup" {
			a.maintenance.RLock()
			defer a.maintenance.RUnlock()
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			a.mu.Lock()
			starting := a.obsLaunch.Phase == "starting" || a.hudImport.Phase == "starting"
			a.mu.Unlock()
			if starting {
				respond(w, http.StatusConflict, map[string]string{"error": "OBS / HUD 正在准备，请等待完成后再操作"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
