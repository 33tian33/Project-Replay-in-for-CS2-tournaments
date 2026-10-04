package replay

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

func hudTestZIP(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hud.zip")
	f, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	for name, body := range files {
		w, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		w.Write([]byte(body))
	}
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	f.Close()
	return p
}
func TestHUDZIPEntriesAndIsolation(t *testing.T) {
	z := hudTestZIP(t, map[string]string{"My HUD/index.html": "<html><head></head><body>HUD</body></html>", "My HUD/assets/hud.js": "console.log(1)", "My HUD/assets/hud.css": "body{background:transparent}"})
	a := testService(t, "agent")
	stage := filepath.Join(t.TempDir(), "extract")
	p, err := unpackHUDZip(z, stage, "my.zip")
	if err != nil {
		t.Fatal(err)
	}
	if p.Entry != "My HUD/index.html" || p.Width != 1920 || !validID(p.ID) {
		t.Fatal(p)
	}
	dest := filepath.Join(a.dir, "hud-packages", p.ID)
	os.MkdirAll(filepath.Dir(dest), 0700)
	if err := os.Rename(stage, dest); err != nil {
		t.Fatal(err)
	}
	for _, asset := range []string{"My%20HUD/index.html", "My%20HUD/assets/hud.js", "__replay_bridge.js"} {
		w := request(t, a, "GET", "/hud-packages/"+p.ID+"/"+asset, nil, "", "null")
		if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox allow-scripts") {
			t.Fatal(asset, w.Code, w.Body.String())
		}
		if strings.HasSuffix(asset, ".html") && !strings.Contains(w.Body.String(), "__replay_bridge.js") {
			t.Fatal("missing data bridge")
		}
	}
	a.previous["b"] = map[string]any{"auth": map[string]any{"token": "private"}, "map": map[string]any{"name": "de_test"}}
	w := request(t, a, "GET", "/hud-data", nil, "", "null")
	if w.Code != 200 || strings.Contains(w.Body.String(), "private") || !strings.Contains(w.Body.String(), "de_test") {
		t.Fatal(w.Body.String())
	}
	if w := request(t, a, "POST", "/api/hud/settings", HUDSettings{}, "", "null"); w.Code != 403 {
		t.Fatal("sandbox HUD could mutate API")
	}
	if w := request(t, a, "GET", "/api/state", nil, "", "null"); w.Code != 403 {
		t.Fatal("sandbox HUD could read control state")
	}
	if w := request(t, a, "GET", "/hud-packages/"+p.ID+"/missing.html", nil, "", ""); w.Code != 404 {
		t.Fatal("missing resource")
	}
}
func TestHUDZIPRejectsUnsafeAndAmbiguousPackages(t *testing.T) {
	for _, files := range []map[string]string{{"../escape": "bad", "index.html": "hi"}, {"/abs/index.html": "hi"}, {"C:\\evil": "hi"}, {"one/index.html": "1", "two/index.html": "2"}, {"package.json": "{}"}, {"replay-hud.json": `{"entry":"../outside.html"}`, "index.html": "x"}} {
		z := hudTestZIP(t, files)
		if _, err := unpackHUDZip(z, filepath.Join(t.TempDir(), "out"), "test"); err == nil {
			t.Fatal("accepted unsafe/ambiguous zip", files)
		}
	}
	p, err := unpackHUDZip(hudTestZIP(t, map[string]string{"bundle/replay-hud.json": `{"name":"Custom","entry":"ui/overlay.html","width":1280,"height":720}`, "bundle/ui/overlay.html": "hi"}), filepath.Join(t.TempDir(), "out"), "zip")
	if err != nil || p.Entry != "bundle/ui/overlay.html" || p.Width != 1280 {
		t.Fatal(p, err)
	}
	// Symlink entries are rejected even when the path itself is harmless.
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	h := &zip.FileHeader{Name: "index.html"}
	h.SetMode(os.ModeSymlink | 0777)
	w, _ := z.CreateHeader(h)
	w.Write([]byte("/etc/passwd"))
	z.Close()
	f := filepath.Join(t.TempDir(), "link.zip")
	os.WriteFile(f, b.Bytes(), 0600)
	if _, err := unpackHUDZip(f, filepath.Join(t.TempDir(), "out"), "link"); err == nil {
		t.Fatal("accepted symlink")
	}
}
func TestHUDZIPActivationAndRollback(t *testing.T) {
	var mu sync.Mutex
	var failInstall bool
	sources := map[string]bool{"Existing HUD": true}
	settings := map[string]any{}
	up := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := up.Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer c.Close()
		c.WriteJSON(map[string]any{"op": 0, "d": map[string]any{}})
		var hello any
		if c.ReadJSON(&hello) != nil {
			return
		}
		c.WriteJSON(map[string]any{"op": 2, "d": map[string]any{}})
		for {
			var req map[string]any
			if c.ReadJSON(&req) != nil {
				return
			}
			d := obj(req, "d")
			args := obj(d, "requestData")
			data := map[string]any{}
			ok := true
			mu.Lock()
			switch stringField(d, "requestType") {
			case "GetCurrentProgramScene":
				data["currentProgramSceneName"] = "Headless"
			case "GetInputList":
				list := []any{}
				for n := range sources {
					list = append(list, map[string]any{"inputName": n, "inputKind": "browser_source"})
				}
				data["inputs"] = list
			case "GetSceneItemList":
				list := []any{}
				for n, on := range sources {
					list = append(list, map[string]any{"sourceName": n, "sceneItemId": n, "sceneItemEnabled": on})
				}
				data["sceneItems"] = list
			case "CreateInput":
				sources[stringField(args, "inputName")] = true
				settings = obj(args, "inputSettings")
				data["sceneItemId"] = 2
			case "GetVideoSettings":
				data["baseWidth"] = 1920
				data["baseHeight"] = 1080
			case "SetSceneItemTransform":
				ok = !failInstall
			case "SetSceneItemEnabled":
				if n, yes := args["sceneItemId"].(string); yes {
					sources[n], _ = args["sceneItemEnabled"].(bool)
				}
			case "RemoveInput":
				delete(sources, stringField(args, "inputName"))
			}
			mu.Unlock()
			c.WriteJSON(map[string]any{"op": 7, "d": map[string]any{"requestId": d["requestId"], "requestStatus": map[string]any{"result": ok, "code": 100}, "responseData": data}})
		}
	}))
	defer server.Close()
	launch := func(context.Context) (string, string, error) {
		return "ws" + strings.TrimPrefix(server.URL, "http"), "", nil
	}
	a := testService(t, "agent")
	a.s.HUD = HUDSettings{Mode: "source", Source: "Existing HUD"}
	a.s.HUDActiveSource = "Existing HUD"
	archive := hudTestZIP(t, map[string]string{"index.html": "<html><head></head><body>Hello</body></html>"})
	p, err := a.activateHUDZipWithOBS(archive, "test.zip", "127.0.0.1:7788", launch)
	if err != nil {
		t.Fatal(err)
	}
	if a.s.HUD.Mode != "package" || a.s.HUDPackage.ID != p.ID || !a.s.TeamHUD {
		t.Fatal("package not activated")
	}
	mu.Lock()
	if sources["Existing HUD"] || !strings.Contains(stringField(settings, "url"), p.ID) {
		t.Error("OBS not switched to package")
	}
	failInstall = true
	mu.Unlock()
	old := a.s.HUDPackage.ID
	if _, err := a.activateHUDZipWithOBS(archive, "broken.zip", "127.0.0.1:7788", launch); err == nil {
		t.Fatal("accepted failed installation")
	}
	if a.s.HUDPackage.ID != old {
		t.Fatal("lost prior HUD")
	}
	a.Close()
	b, e := New(a.dir, "agent")
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	raw, _ := json.Marshal(b.s.HUDPackage)
	if !strings.Contains(string(raw), p.ID) {
		t.Fatal("not persisted")
	}
}
