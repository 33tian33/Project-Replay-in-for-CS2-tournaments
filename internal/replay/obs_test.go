package replay

import (
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestOBSAuthenticationAndRequests(t *testing.T) {
	up := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, e := up.Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer conn.Close()
		conn.WriteJSON(map[string]any{"op": 0, "d": map[string]any{"authentication": map[string]string{"salt": "salt", "challenge": "challenge"}}})
		var hello map[string]any
		if conn.ReadJSON(&hello) != nil {
			return
		}
		if stringField(obj(hello, "d"), "authentication") != digest(digest("passwordsalt")+"challenge") {
			return
		}
		conn.WriteJSON(map[string]any{"op": 2, "d": map[string]any{}})
		var req map[string]any
		if conn.ReadJSON(&req) != nil {
			return
		}
		data := obj(req, "d")
		conn.WriteJSON(map[string]any{"op": 7, "d": map[string]any{"requestId": data["requestId"], "requestStatus": map[string]any{"result": true, "code": 100}, "responseData": map[string]any{"obsVersion": "test"}}})
	}))
	defer server.Close()
	c := defaults()
	c.OBSURL = "ws" + strings.TrimPrefix(server.URL, "http")
	c.OBSPassword = "password"
	o, e := openOBS(c)
	if e != nil {
		t.Fatal(e)
	}
	defer o.close()
	r, e := o.call("GetVersion", nil)
	if e != nil || r["obsVersion"] != "test" {
		t.Fatal(r, e)
	}
}

func TestHUDInstallationInOBSIsIdempotent(t *testing.T) {
	up := websocket.Upgrader{}
	// All mock state is confined to the handler goroutine; each client finishes
	// its connection before the following installation request.
	var mu sync.Mutex
	var created int
	var settings map[string]any
	var transform map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.WriteJSON(map[string]any{"op": 0, "d": map[string]any{}})
		var hello map[string]any
		if conn.ReadJSON(&hello) != nil {
			return
		}
		conn.WriteJSON(map[string]any{"op": 2, "d": map[string]any{}})
		for {
			var req map[string]any
			if conn.ReadJSON(&req) != nil {
				return
			}
			d := obj(req, "d")
			args := obj(d, "requestData")
			data := map[string]any{}
			mu.Lock()
			switch stringField(d, "requestType") {
			case "GetCurrentProgramScene":
				data["currentProgramSceneName"] = "Capture"
			case "GetInputList":
				data["inputs"] = []any{}
				if created > 0 {
					data["inputs"] = []any{map[string]any{"inputName": "Project Replay Team HUD"}}
				}
			case "CreateInput":
				created++
				settings = obj(args, "inputSettings")
				data["sceneItemId"] = 7
			case "SetInputSettings":
				settings = obj(args, "inputSettings")
			case "GetSceneItemId":
				data["sceneItemId"] = 7
			case "GetSceneItemList":
				data["sceneItems"] = []any{map[string]any{}, map[string]any{}}
			case "GetVideoSettings":
				data["baseWidth"] = 1280
				data["baseHeight"] = 720
			case "SetSceneItemTransform":
				transform = obj(args, "sceneItemTransform")
			}
			mu.Unlock()
			conn.WriteJSON(map[string]any{"op": 7, "d": map[string]any{"requestId": d["requestId"], "requestStatus": map[string]any{"result": true, "code": 100}, "responseData": data}})
		}
	}))
	defer server.Close()
	a := testService(t, "agent")
	a.s.Config.OBSURL = "ws" + strings.TrimPrefix(server.URL, "http")
	for i := 0; i < 2; i++ {
		w := request(t, a, "POST", "http://127.0.0.1:7788/api/hud/obs", map[string]any{}, "", "")
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if created != 1 || settings["url"] != "http://127.0.0.1:7788/hud.html" || number(transform, "scaleX") != 1280.0/1920 {
		t.Fatal(created, settings, transform)
	}
}
