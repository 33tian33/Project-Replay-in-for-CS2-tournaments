package replay

import "testing"

func TestAutoCaptureDefaultAndCalibration(t *testing.T) {
	a := testService(t, "director")
	if !a.s.Config.AutoCapture || a.s.Config.Strict {
		t.Fatal("automatic near-time capture not default")
	}
	a.s.Config.Mode = "live"
	w := request(t, a, "POST", "/api/calibrate", map[string]float64{"delta": 8, "uncertainty": .1}, "", "")
	if w.Code != 200 || a.s.Config.Paused {
		t.Fatal(w.Body.String())
	}
	request(t, a, "POST", "/api/pause", map[string]bool{"paused": true}, "", "")
	request(t, a, "POST", "/api/calibrate", map[string]float64{"delta": 8, "uncertainty": .1}, "", "")
	if !a.s.Config.Paused || a.s.Config.AutoCapture {
		t.Fatal("manual pause overridden")
	}
	epoch := a.s.Config.Epoch
	request(t, a, "POST", "/api/auto-capture", map[string]bool{"enabled": true}, "", "")
	if a.s.Config.Paused || a.s.Config.Epoch != epoch {
		t.Fatal("auto capture failed or epoch changed")
	}
}
func TestAutoCaptureDoesNotWaitForCalibration(t *testing.T) {
	a := testService(t, "agent")
	a.s.Config.Mode = "live"
	a.s.Config.CalibratedUntil = 0
	request(t, a, "POST", "/api/auto-capture", map[string]bool{"enabled": true}, "", "")
	if a.s.Config.Paused || !a.s.Config.AutoCapture {
		t.Fatal("automatic round-clock capture blocked")
	}
	if w := request(t, a, "POST", "/api/auto-capture", map[string]bool{"enabled": true}, "", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
}
