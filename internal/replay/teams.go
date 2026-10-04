package replay

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strings"
)

type TeamIdentity struct {
	Name string `json:"name"`
	Logo string `json:"logo"`
}
type TeamSettings struct {
	CT                 TeamIdentity `json:"ct"`
	T                  TeamIdentity `json:"t"`
	HalfRounds         int          `json:"half_rounds"`
	OvertimeHalfRounds int          `json:"overtime_half_rounds"`
}

func (s TeamSettings) validate() error {
	if s.HalfRounds < 0 || s.HalfRounds > 100 || s.OvertimeHalfRounds < 0 || s.OvertimeHalfRounds > 100 {
		return errors.New("换边回合数必须为 1–100（0 使用默认 12 / 3）")
	}
	for _, t := range []TeamIdentity{s.CT, s.T} {
		if len(t.Name) > 200 {
			return errors.New("战队名称过长")
		}
		if t.Logo == "" {
			continue
		}
		parts := strings.SplitN(t.Logo, ",", 2)
		if len(parts) != 2 || (parts[0] != "data:image/png;base64" && parts[0] != "data:image/jpeg;base64") {
			return errors.New("图标须为 PNG 或 JPEG")
		}
		b, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil || len(b) > 256<<10 {
			return errors.New("图标格式错误或超过 256 KB")
		}
		c, format, err := image.DecodeConfig(bytes.NewReader(b))
		if err != nil || c.Width > 2048 || c.Height > 2048 || c.Width < 1 || c.Height < 1 || parts[0] != "data:image/"+format+";base64" {
			return errors.New("图标须为有效的 PNG / JPEG，尺寸不超过 2048×2048")
		}
	}
	return nil
}
func (s TeamSettings) swapped(round int) bool {
	half, ot := s.HalfRounds, s.OvertimeHalfRounds
	if half == 0 {
		half = 12
	}
	if ot == 0 {
		ot = 3
	}
	played := max(0, round-1)
	if played < half*2 {
		return played >= half
	}
	// First overtime continues the second-half sides; switch every OT half.
	return ((played-half*2)/ot)%2 == 0
}
func (a *Service) teamRoutes(mux, api *http.ServeMux) {
	api.HandleFunc("POST /api/teams", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("agent") == "1" && a.role != "agent" {
			fail(w, errors.New("同步目标必须是 Linux Agent"))
			return
		}
		var s TeamSettings
		if err := decode(w, r, &s); err != nil {
			fail(w, err)
			return
		}
		if err := s.validate(); err != nil {
			fail(w, err)
			return
		}
		a.mu.Lock()
		a.s.Config.Teams = s
		a.saveLocked()
		storageErr := a.storageErr
		c := a.s.Config
		a.mu.Unlock()
		if storageErr != "" {
			fail(w, errors.New(storageErr))
			return
		}
		if a.role == "director" && remoteConfigured(c) {
			if err := remoteCall(r.Context(), c, "POST", "/api/teams?agent=1", s, nil); err != nil {
				fail(w, fmt.Errorf("本机战队设置已保存，Linux 同步失败，请重试：%w", err))
				return
			}
		}
		respond(w, 200, map[string]bool{"ok": true, "synced": a.role == "director" && remoteConfigured(c)})
	})
	mux.HandleFunc("GET /hud-api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		a.mu.Lock()
		defer a.mu.Unlock()
		side := "b"
		if a.role == "director" {
			side = "a"
		}
		p := a.previous[side]
		m := obj(p, "map")
		s := a.s.Config.Teams
		round := a.gsiRound[side] + 1
		ct, tt := s.CT, s.T
		if s.swapped(round) {
			ct, tt = tt, ct
		}
		if ct.Name == "" {
			ct.Name = stringField(obj(m, "team_ct"), "name")
			if ct.Name == "" {
				ct.Name = "CT"
			}
		}
		if tt.Name == "" {
			tt.Name = stringField(obj(m, "team_t"), "name")
			if tt.Name == "" {
				tt.Name = "T"
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		respond(w, 200, map[string]any{"ct": ct, "t": tt, "ct_score": number(obj(m, "team_ct"), "score"), "t_score": number(obj(m, "team_t"), "score"), "round": round, "clock": a.roundClocks[side].Clock, "fresh": nowMS()-a.gsiSeen[side] < 2000, "visible": !a.hudHidden})
	})
}
