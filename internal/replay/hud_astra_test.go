package replay

import (
	"strings"
	"testing"
)

func TestBundledAstraAssetsAndIsolation(t *testing.T) {
	a := testService(t, "agent")
	if a.s.HUD.Mode != "astra" {
		t.Fatal("Astra must be the default HUD", a.s.HUD)
	}
	for _, path := range []string{"/astra/index.html", "/astra/replay-bridge.js", "/astra/assets/fonts/rajdhani-600.woff2", "/astra/assets/radars/de_ancient.png", "/astra/assets/weapons/ak47.svg"} {
		w := request(t, a, "GET", path, nil, "", "null")
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("missing embedded asset %s: %d", path, w.Code)
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox allow-scripts") {
			t.Fatal("HUD is not isolated")
		}
	}
	w := request(t, a, "GET", "/astra/index.html", nil, "", "")
	if !strings.Contains(w.Body.String(), `src="/astra/replay-bridge.js"`) || strings.Contains(w.Body.String(), "const socket = io()") {
		t.Fatal("Astra still depends on external Socket.IO")
	}
	if w := request(t, a, "POST", "/api/hud/settings", HUDSettings{Mode: "astra"}, "", "null"); w.Code != 403 {
		t.Fatal("HUD sandbox can mutate control settings")
	}
}
