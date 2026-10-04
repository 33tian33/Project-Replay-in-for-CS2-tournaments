package replay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

func TestCustomHUDValidationAndPersistence(t *testing.T) {
	for _, h := range []HUDSettings{{Mode: "other"}, {Mode: "url", URL: "file:///tmp/hud.html"}, {Mode: "url", URL: "javascript:alert(1)"}, {Mode: "url", URL: "http://user:pass@localhost"}, {Mode: "source"}, {Mode: "source", Source: "Project Replay Team HUD"}, {Width: 99999}} {
		if h.validate() == nil {
			t.Fatal("accepted", h)
		}
	}
	a := testService(t, "agent")
	h := HUDSettings{Mode: "url", URL: "http://127.0.0.1:1350/recorder", KeepNative: true}
	if w := request(t, a, "POST", "/api/hud/settings", h, "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a.Close()
	b, err := New(a.dir, "agent")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.s.HUD.URL != h.URL || !b.s.HUD.KeepNative || b.s.HUD.Width != 1920 {
		t.Fatal("settings not persisted", b.s.HUD)
	}
	b.active = true
	if w := request(t, b, "POST", "/api/hud/settings", HUDSettings{}, "", ""); w.Code != 400 {
		t.Fatal("allowed during capture")
	}
	d := testService(t, "director")
	if w := request(t, d, "POST", "/api/hud/settings", h, "", ""); w.Code != 400 {
		t.Fatal("director accepted settings")
	}
}

func TestCustomHUDSwitching(t *testing.T) {
	var mu sync.Mutex
	inputs := map[string]map[string]any{"OpenHUD Broadcast": {"inputName": "OpenHUD Broadcast", "inputKind": "browser_source"}}
	ids := map[string]int{"OpenHUD Broadcast": 1}
	enabled := map[int]bool{1: true}
	settings := map[string]map[string]any{}
	transforms := map[int]map[string]any{}
	active := false
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
			case "GetRecordStatus", "GetStreamStatus":
				data["outputActive"] = active
			case "GetCurrentProgramScene":
				data["currentProgramSceneName"] = "Headless"
			case "GetInputList":
				list := []any{}
				for _, v := range inputs {
					list = append(list, v)
				}
				data["inputs"] = list
			case "CreateInput":
				name := stringField(args, "inputName")
				ids[name] = len(ids) + 1
				inputs[name] = map[string]any{"inputName": name, "inputKind": "browser_source"}
				settings[name] = obj(args, "inputSettings")
				data["sceneItemId"] = ids[name]
			case "SetInputSettings":
				settings[stringField(args, "inputName")] = obj(args, "inputSettings")
			case "GetSceneItemId":
				id, found := ids[stringField(args, "sourceName")]
				ok = found
				data["sceneItemId"] = id
			case "GetSceneItemList":
				list := []any{}
				for n, id := range ids {
					list = append(list, map[string]any{"sourceName": n, "sceneItemId": id})
				}
				data["sceneItems"] = list
			case "SetSceneItemEnabled":
				enabled[int(number(args, "sceneItemId"))], _ = args["sceneItemEnabled"].(bool)
			case "GetVideoSettings":
				data["baseWidth"] = 1920
				data["baseHeight"] = 1080
			case "SetSceneItemTransform":
				transforms[int(number(args, "sceneItemId"))] = obj(args, "sceneItemTransform")
			}
			mu.Unlock()
			c.WriteJSON(map[string]any{"op": 7, "d": map[string]any{"requestId": d["requestId"], "requestStatus": map[string]any{"result": ok, "code": 100}, "responseData": data}})
		}
	}))
	defer server.Close()
	a := testService(t, "agent")
	a.s.Config.OBSURL = "ws" + strings.TrimPrefix(server.URL, "http")
	apply := func(h HUDSettings) {
		t.Helper()
		a.s.HUD = h
		if w := request(t, a, "POST", "http://127.0.0.1:7788/api/hud/obs", map[string]any{}, "", ""); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	apply(HUDSettings{})
	apply(HUDSettings{Mode: "url", URL: "http://127.0.0.1:1350/recorder", Width: 1280, Height: 720})
	mu.Lock()
	if enabled[ids["Project Replay Team HUD"]] || settings["Project Replay Custom HUD"]["url"] != "http://127.0.0.1:1350/recorder" || number(transforms[ids["Project Replay Custom HUD"]], "scaleX") != 1.5 {
		t.Error("custom browser install or builtin hiding failed")
	}
	mu.Unlock()
	apply(HUDSettings{Mode: "source", Source: "OpenHUD Broadcast", KeepNative: true})
	mu.Lock()
	if !enabled[1] || settings["OpenHUD Broadcast"] != nil || transforms[1] != nil || enabled[ids["Project Replay Custom HUD"]] {
		t.Error("existing source mutated or prior overlay still visible")
	}
	mu.Unlock()
	if a.s.TeamHUD || a.s.HUDActiveSource != "OpenHUD Broadcast" {
		t.Fatal("wrong HUD preference")
	}
	apply(HUDSettings{})
	mu.Lock()
	if enabled[1] {
		t.Error("external source not disabled on return to builtin")
	}
	active = true
	before := len(inputs)
	mu.Unlock()
	a.s.HUD = HUDSettings{Mode: "url", URL: "http://localhost:1350/recorder"}
	if w := request(t, a, "POST", "http://127.0.0.1:7788/api/hud/obs", map[string]any{}, "", ""); w.Code != 400 {
		t.Fatal("modified recording OBS")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(inputs) != before {
		t.Fatal("changed sources during recording")
	}
}
