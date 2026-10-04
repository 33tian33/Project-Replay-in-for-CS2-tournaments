package replay

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type OutputSettings struct {
	AutoRound   bool   `json:"auto_round"`
	AutoFull    bool   `json:"auto_full"`
	AutoHalf    bool   `json:"auto_half"`
	HalfRound   int    `json:"half_round"`
	Delay       int    `json:"delay"`
	Hotkey      string `json:"hotkey"`
	Transition1 string `json:"transition1"`
	Transition2 string `json:"transition2"`
}

func defaultOutput() OutputSettings {
	return OutputSettings{AutoRound: true, AutoHalf: true, AutoFull: true, HalfRound: 12, Delay: 3, Hotkey: "Ctrl+Alt+R"}
}

type OutputItem struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}
type OutputSession struct {
	ID       string       `json:"id"`
	Kind     string       `json:"kind"`
	Round    int          `json:"round"`
	Items    []OutputItem `json:"items"`
	Index    int          `json:"index"`
	Due      int64        `json:"due"`
	Deadline int64        `json:"deadline"`
}

func (a *Service) currentArtifactLocked(v Artifact) bool {
	for _, j := range a.s.Jobs {
		if j.ID == v.JobID {
			return j.Match == a.s.Config.Match && j.Map == a.s.Config.Map && j.Epoch == a.s.Config.Epoch
		}
	}
	return false
}
func appendUnique(ids []string, id string) []string {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}
func (a *Service) outputIDsLocked(kind string) []string {
	switch kind {
	case "half":
		return a.s.HalfQueue
	case "full":
		return a.s.FullQueue
	default:
		return a.s.Queue
	}
}
func (a *Service) outputIncludesRound(kind string, round, candidate int) bool {
	if kind == "round" {
		return candidate == round
	}
	if kind == "half" {
		return candidate <= a.s.Output.HalfRound
	}
	return true
}
func (a *Service) pendingOutputLocked(kind string, round int) bool {
	if kind == "round" {
		return a.pendingRoundLocked(round)
	}
	rounds := map[int]bool{}
	for _, e := range a.s.Events {
		rounds[e.Round] = true
	}
	for _, j := range a.s.Jobs {
		for _, e := range j.Events {
			rounds[e.Round] = true
		}
	}
	for r := range rounds {
		if a.outputIncludesRound(kind, round, r) && a.pendingRoundLocked(r) {
			return true
		}
	}
	return false
}
func (a *Service) startOutputLocked(kind string, round int) error {
	if a.role != "director" {
		return errors.New("仅导播端可播出")
	}
	if a.output.ID != "" {
		return errors.New("已有回放或待播触发；请先返回直播")
	}
	if nowMS()-a.outputSeen > 3000 {
		return errors.New("OBS 浏览器源未连接，请添加输出地址并保持启用")
	}
	if kind != "round" && kind != "half" && kind != "full" {
		return errors.New("未知播出类型")
	}
	ids := a.outputIDsLocked(kind)
	items := []OutputItem{}
	for _, key := range ids {
		for _, v := range a.s.Artifacts {
			if v.ID == key && a.currentArtifactLocked(v) && a.outputIncludesRound(kind, round, v.Round) {
				items = append(items, OutputItem{v.ID, "replay"})
			}
		}
	}
	if len(items) == 0 && !a.pendingOutputLocked(kind, round) {
		return errors.New("所选列表没有当前会话的就绪素材")
	}
	if a.s.Output.Transition1 != "" {
		items = append([]OutputItem{{a.s.Output.Transition1, "transition"}}, items...)
	}
	if a.s.Output.Transition2 != "" && !a.pendingOutputLocked(kind, round) {
		items = append(items, OutputItem{a.s.Output.Transition2, "transition"})
	}
	a.output = OutputSession{ID: id(), Kind: kind, Round: round, Items: items, Deadline: nowMS() + 120000}
	a.outputError = ""
	a.logLocked("info", fmt.Sprintf("网页播出：%s R%d，%d 个媒体项", kind, round, len(items)))
	return nil
}
func (a *Service) manualOutputLocked(kind string, round int) error {
	previous := a.output
	if previous.Due > 0 {
		a.output = OutputSession{}
	}
	if err := a.startOutputLocked(kind, round); err != nil {
		a.output = previous
		return err
	}
	if kind == "round" {
		if a.manualRounds == nil {
			a.manualRounds = map[int]bool{}
		}
		a.manualRounds[round] = true
	}
	return nil
}

// Pending observations stay behind ready media, including events without a job yet.
func (a *Service) pendingRoundLocked(round int) bool {
	for _, e := range a.s.Events {
		if e.Round == round && e.Epoch == a.s.Config.Epoch && e.Match == a.s.Config.Match && e.Map == a.s.Config.Map && !terminal(e.Status) && e.Status != "CONFLICT" && e.Status != "UNCERTAIN" && e.Status != "PIN_CONFLICT" {
			return true
		}
	}
	for _, j := range a.s.Jobs {
		if j.Epoch != a.s.Config.Epoch || j.Match != a.s.Config.Match || j.Map != a.s.Config.Map || terminal(j.Status) {
			continue
		}
		for _, e := range j.Events {
			if e.Round == round {
				return true
			}
		}
	}
	return false
}

func (a *Service) appendOutputLocked() {
	o := &a.output
	if o.ID == "" || o.Due > 0 {
		return
	}
	// Do not change a transition that has already begun.
	if o.Index < len(o.Items) && o.Items[o.Index].Kind == "transition" && o.Index > 0 {
		return
	}
	seen := map[string]bool{}
	for _, item := range o.Items {
		seen[item.ID] = true
	}
	tail := len(o.Items)
	if tail > 0 && o.Items[tail-1].Kind == "transition" && tail-1 > o.Index {
		tail--
	}
	additions := []OutputItem{}
	for _, key := range a.outputIDsLocked(o.Kind) {
		for _, v := range a.s.Artifacts {
			if v.ID == key && a.outputIncludesRound(o.Kind, o.Round, v.Round) && a.currentArtifactLocked(v) && !seen[key] {
				additions = append(additions, OutputItem{key, "replay"})
				seen[key] = true
			}
		}
	}
	suffix := append([]OutputItem(nil), o.Items[tail:]...)
	o.Items = append(append(o.Items[:tail], additions...), suffix...)
	if !a.pendingOutputLocked(o.Kind, o.Round) && a.s.Output.Transition2 != "" && !seen[a.s.Output.Transition2] {
		o.Items = append(o.Items, OutputItem{a.s.Output.Transition2, "transition"})
	}
	if o.Index >= len(o.Items) && !a.pendingOutputLocked(o.Kind, o.Round) {
		a.output = OutputSession{}
	}
}

func (a *Service) scheduleOutputLocked(kind string, round int) {
	if a.output.ID != "" {
		a.logLocked("warn", "自动回放被跳过：已有播出会话")
		return
	}
	a.output = OutputSession{ID: id(), Kind: kind, Round: round, Due: nowMS() + int64(a.s.Output.Delay)*1000, Deadline: nowMS() + int64(a.s.Output.Delay)*1000 + 30000}
}
func (a *Service) tickOutputLocked() {
	a.appendOutputLocked()
	o := a.output
	if o.ID == "" {
		return
	}
	if o.Due > 0 {
		if nowMS() < o.Due {
			return
		}

		a.output = OutputSession{}
		if e := a.startOutputLocked(o.Kind, o.Round); e != nil {
			a.outputError = e.Error()
			a.logLocked("warn", "自动回放未播出："+e.Error())
		}
		return
	}
	if nowMS()-a.outputSeen > 3000 || nowMS() > o.Deadline {
		a.output = OutputSession{}
		a.outputError = "输出断开或媒体超时，已恢复透明"
		a.logLocked("warn", a.outputError)
	}
}
func (a *Service) detectOutputLocked(p map[string]any) {
	if a.role != "director" {
		return
	}
	phase := stringField(obj(p, "round"), "phase")
	mapPhase := stringField(obj(p, "map"), "phase")
	old := a.previous["a"]
	if old == nil {
		return
	} // Initial snapshots must never trigger a stale replay.
	oldPhase := stringField(obj(old, "round"), "phase")
	round := int(number(obj(p, "map"), "round")) + 1
	// The final snapshot may already carry the incremented completed-round counter.
	if oldPhase == "live" {
		round = int(number(obj(old, "map"), "round")) + 1
	}
	if phase == "live" && a.output.Kind == "round" && a.output.ID != "" && int(number(obj(p, "map"), "round"))+1 > a.output.Round {
		clock := readRoundClock(p)
		cut := clock != nil && clock.Phase == "live" && clock.Remaining <= 107
		for steam, raw := range obj(p, "allplayers") {
			player, _ := raw.(map[string]any)
			before, _ := obj(old, "allplayers")[steam].(map[string]any)
			if kills, ok := gsiCount(player, "state", "round_kills"); ok && kills > 0 {
				cut = true
			}
			if int(number(obj(p, "map"), "round")) == int(number(obj(old, "map"), "round")) {
				if diff, _, _, ok := gsiKillDelta(before, player); ok && diff > 0 {
					cut = true
				}
			}
		}
		if cut {
			a.output = OutputSession{}
		}
	}
	if mapPhase == "gameover" {
		if stringField(obj(old, "map"), "phase") != "gameover" && !a.fullTriggered && a.s.Output.AutoFull {
			a.fullTriggered = true
			a.output = OutputSession{}
			a.scheduleOutputLocked("full", round)
		}
		return
	}
	if mapPhase == "warmup" {
		return
	}
	halfSignal := (stringField(obj(p, "phase_countdowns"), "phase") == "halftime" && stringField(obj(old, "phase_countdowns"), "phase") != "halftime") || (mapPhase == "intermission" && stringField(obj(old, "map"), "phase") != "intermission")
	ended := phase == "over" && oldPhase != "over"
	if !a.halfTriggered && a.s.Output.AutoHalf && (halfSignal || ended && round == a.s.Output.HalfRound) {
		a.halfTriggered = true
		if a.output.Kind == "round" {
			a.output = OutputSession{}
		}
		a.scheduleOutputLocked("half", round)
		a.endRound = round
	} else if ended && round > a.endRound {
		a.endRound = round
		if a.s.Output.AutoRound && !a.manualRounds[round] {
			a.scheduleOutputLocked("round", round)
		}
	}
}
func (a *Service) outputRoutes(mux, api *http.ServeMux) {
	api.HandleFunc("POST /api/output/settings", func(w http.ResponseWriter, r *http.Request) {
		var p OutputSettings
		if e := decode(w, r, &p); e != nil {
			fail(w, e)
			return
		}
		if p.HalfRound < 1 || p.HalfRound > 100 || p.Delay < 0 || p.Delay > 60 {
			fail(w, errors.New("半场回合应为 1–100，延迟应为 0–60 秒"))
			return
		}
		if _, _, e := parseHotkey(p.Hotkey); e != nil {
			fail(w, e)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		p.Transition1 = a.s.Output.Transition1
		p.Transition2 = a.s.Output.Transition2
		if p.HalfRound != a.s.Output.HalfRound {
			kept := []string{}
			for _, key := range a.s.HalfQueue {
				for _, v := range a.s.Artifacts {
					if v.ID == key && a.currentArtifactLocked(v) && v.Round <= p.HalfRound {
						kept = append(kept, key)
					}
				}
			}
			for _, v := range a.s.Artifacts {
				if a.currentArtifactLocked(v) && v.Round > a.s.Output.HalfRound && v.Round <= p.HalfRound {
					kept = appendUnique(kept, v.ID)
				}
			}
			a.s.HalfQueue = kept
		}
		a.s.Output = p
		a.saveLocked()
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/output/transition/{slot}", func(w http.ResponseWriter, r *http.Request) {
		slot := r.PathValue("slot")
		if slot != "1" && slot != "2" {
			http.NotFound(w, r)
			return
		}
		ext := strings.ToLower(filepath.Ext(r.URL.Query().Get("name")))
		if ext != ".mp4" && ext != ".webm" {
			fail(w, errors.New("请导入 MP4 或 WebM 视频；透明转场使用带 Alpha 的 WebM"))
			return
		}
		key := id() + ext
		path := filepath.Join(a.dir, "media", key)
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			fail(w, e)
			return
		}
		_, e = io.Copy(f, http.MaxBytesReader(w, r.Body, 256<<20))
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			os.Remove(path)
			fail(w, e)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if slot == "1" {
			a.s.Output.Transition1 = key
		} else {
			a.s.Output.Transition2 = key
		}
		a.saveLocked()
		respond(w, 200, map[string]string{"id": key})
	})
	api.HandleFunc("POST /api/output/play", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Kind  string `json:"kind"`
			Round int    `json:"round"`
		}
		if e := decode(w, r, &p); e != nil {
			fail(w, e)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if e := a.manualOutputLocked(p.Kind, p.Round); e != nil {
			fail(w, e)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/output/stop", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.output = OutputSession{}
		a.mu.Unlock()
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/output/queue", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Kind string   `json:"kind"`
			IDs  []string `json:"ids"`
		}
		if e := decode(w, r, &p); e != nil {
			fail(w, e)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if p.Kind != "round" && p.Kind != "half" && p.Kind != "full" {
			fail(w, errors.New("未知列表"))
			return
		}
		available := map[string]bool{}
		for _, v := range a.s.Artifacts {
			if a.currentArtifactLocked(v) {
				available[v.ID] = true
			}
		}
		for _, key := range p.IDs {
			if !available[key] {
				fail(w, errors.New("列表含重复素材或非当前会话素材"))
				return
			}
			delete(available, key)
		}
		if p.Kind == "half" {
			a.s.HalfQueue = p.IDs
		} else if p.Kind == "full" {
			a.s.FullQueue = p.IDs
		} else {
			a.s.Queue = p.IDs
		}
		a.saveLocked()
		respond(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/output-api/", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.URL.Path == "/output-api/state" && r.Method == "GET":
			client := r.URL.Query().Get("client")
			if !validID(client) {
				http.Error(w, "invalid client", 400)
				return
			}
			if a.outputClient != "" && a.outputClient != client && nowMS()-a.outputSeen < 3000 {
				http.Error(w, "output already connected", 409)
				return
			}
			a.outputClient = client
			a.outputSeen = nowMS()
			respond(w, 200, a.output)
		case r.URL.Path == "/output-api/ack" && r.Method == "POST":
			var p struct {
				ID     string `json:"id"`
				Index  int    `json:"index"`
				Client string `json:"client"`
				Error  string `json:"error"`
			}
			if e := decode(w, r, &p); e != nil {
				fail(w, e)
				return
			}
			if p.Client != a.outputClient || p.ID != a.output.ID || p.Index != a.output.Index || a.output.Due > 0 || a.output.ID == "" {
				http.Error(w, "stale acknowledgement", 409)
				return
			}
			if p.Error != "" {
				a.outputError = "媒体播放失败，已恢复透明"
				if p.Error == "autoplay" {
					a.outputError = "浏览器阻止自动播放，请使用 OBS 浏览器源；普通浏览器需先点击输出页"
				}
				if p.Error == "load timeout" {
					a.outputError = "媒体加载超时，已恢复透明"
				}
				a.logLocked("warn", a.outputError)
				a.output = OutputSession{}
			} else {
				a.output.Index++
				a.output.Deadline = nowMS() + 120000
				a.appendOutputLocked()
				if a.output.ID != "" && a.output.Index >= len(a.output.Items) && !a.pendingOutputLocked(a.output.Kind, a.output.Round) {
					a.output = OutputSession{}
				}
			}
			respond(w, 200, map[string]bool{"ok": true})
		case strings.HasPrefix(r.URL.Path, "/output-api/media/") && (r.Method == "GET" || r.Method == "HEAD"):
			key := strings.TrimPrefix(r.URL.Path, "/output-api/media/")
			path := ""
			// Only media present in the active immutable playlist is available to this output.
			for _, item := range a.output.Items {
				if item.ID != key {
					continue
				}
				if item.Kind == "transition" {
					path = filepath.Join(a.dir, "media", item.ID)
				} else {
					for _, v := range a.s.Artifacts {
						if v.ID == key {
							path = v.Path
						}
					}
				}
			}
			if path == "" {
				http.NotFound(w, r)
				return
			}
			// Release the mutex while a potentially large Range response is streamed.
			a.mu.Unlock()
			http.ServeFile(w, r, path)
			a.mu.Lock()
		default:
			http.NotFound(w, r)
		}
	})
}
