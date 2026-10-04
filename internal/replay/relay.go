package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"projectreplay/internal/relay"
	"strconv"
	"strings"
	"time"
)

type relayStatus struct {
	PeerOnline bool   `json:"peer_online"`
	Online     bool   `json:"online"`
	Peer       string `json:"peer"`
	Pending    string `json:"pending"`
	Error      string `json:"error"`
}

func remoteConfigured(c Config) bool { return c.ConnectionMode == "relay" || c.WorkerURL != "" }
func relayIdentity(c Config) string {
	return strings.Join([]string{c.ConnectionMode, c.RelayURL, c.RelayDevice, c.RelayGroup, c.RelayName, strconv.FormatBool(c.RelayAutoPair)}, "\x00")
}
func (a *Service) relayLoop() {
	defer a.wg.Done()
	var identity string
	var cancel context.CancelFunc
	var done chan struct{}
	var retry time.Time
	var sentGOTV string
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	stop := func() {
		if cancel != nil {
			cancel()
			<-done
			cancel = nil
		}
	}
	defer stop()
	for {
		a.mu.Lock()
		c := a.s.Config
		a.mu.Unlock()
		key := relayIdentity(c)
		if key != identity {
			stop()
			identity = key
			retry = time.Time{}
			a.mu.Lock()
			a.relayState = relayStatus{}
			a.mu.Unlock()
		}
		if cancel != nil {
			select {
			case <-done:
				cancel()
				cancel = nil
				retry = time.Now().Add(3 * time.Second)
			default:
			}
		}
		if c.ConnectionMode == "relay" && cancel == nil && !time.Now().Before(retry) {
			var ctx context.Context
			ctx, cancel = context.WithCancel(a.ctx)
			done = make(chan struct{})
			go func(c Config, key string, done chan struct{}) {
				defer close(done)
				err := relay.Run(ctx, c.RelayURL, c.RelayDevice, relay.Hello{Role: a.role, Group: c.RelayGroup, Name: c.RelayName, Auto: c.RelayAutoPair}, func(n relay.Node) {
					a.mu.Lock()
					defer a.mu.Unlock()
					if relayIdentity(a.s.Config) != key {
						return
					}
					a.relayState = relayStatus{Online: true, PeerOnline: n.PeerOnline, Peer: n.Peer, Pending: n.Pending}
					if a.s.Config.RelayPair != n.Pair {
						a.s.Config.RelayPair = n.Pair
						a.remoteAt = 0
						a.saveLocked()
					}
				}, func(ctx context.Context, m relay.Message) relay.Message { return a.relayCommand(ctx, m) }, func(ctx context.Context, m relay.Message) { a.uploadRelayMedia(ctx, c, m) })
				a.mu.Lock()
				if relayIdentity(a.s.Config) == key {
					a.relayState.Online = false
					if err != nil && ctx.Err() == nil {
						a.relayState.Error = "云中继连接断开，正在重试；请检查网络与群组码"
					}
				}
				a.mu.Unlock()
			}(c, key, done)
		}

		if c.ConnectionMode == "relay" && c.RelayPair != "" && a.role == "director" && c.AutoSendGOTV && c.GOTV != "" && c.Mode == "live" {
			sendKey := c.RelayPair + "\x00" + c.GOTV + "\x00" + c.GOTVPassword
			if sendKey != sentGOTV {
				sentGOTV = sendKey // Never blindly repeat a potentially executed connect command.
				err := remoteCall(a.ctx, c, "POST", "/api/connect-gotv?agent=1", gotvRequest{GOTV: c.GOTV, Password: &c.GOTVPassword}, nil)
				a.mu.Lock()
				if err != nil {
					a.logLocked("warn", "中继 GOTV 自动发送未确认，请核对录制机后手动连接")
				} else {
					a.logLocked("info", "已通过云中继发送 GOTV 连接信息")
				}
				a.mu.Unlock()
			}
		}
		select {
		case <-a.ctx.Done():
			if cancel != nil {
				cancel()
			}
			return
		case <-tick.C:
		}
	}
}
func (a *Service) relayCommand(ctx context.Context, m relay.Message) relay.Message {
	if a.role != "agent" || !relay.Allowed(m.Method, m.Path) {
		return relay.Message{Status: 403, Body: []byte(`{"error":"禁止转发此接口"}`)}
	}
	a.mu.Lock()
	validPair := a.s.Config.ConnectionMode == "relay" && a.s.Config.RelayPair != "" && a.s.Config.RelayPair == m.Pair
	a.mu.Unlock()
	if !validPair {
		return relay.Message{Status: 409, Body: []byte(`{"error":"本机配对已变化"}`)}
	}
	if ctx.Err() != nil {
		return relay.Message{Status: 408, Body: []byte(`{"error":"指令已过期"}`)}
	}
	// Execute the existing, allowlisted local API in process; no arbitrary network destination.
	r := httptest.NewRequest(m.Method, m.Path, bytes.NewReader(m.Body)).WithContext(ctx)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Body.Len() > 8<<20 {
		return relay.Message{Status: 502, Body: []byte(`{"error":"节点响应过大"}`)}
	}
	return relay.Message{Status: w.Code, Body: w.Body.Bytes()}
}

var relayControlHTTP = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
var relayMediaHTTP = &http.Client{Timeout: 6 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func relayRequest(ctx context.Context, c Config, method, path string, body io.Reader) (*http.Request, error) {
	if err := relay.ValidateURL(c.RelayURL); err != nil {
		return nil, err
	}
	r, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.RelayURL, "/")+path, body)
	if e != nil {
		return nil, e
	}
	relay.Headers(r, c.RelayDevice, c.RelayGroup, c.RelayPair)
	r.Header.Set("Content-Type", "application/json")
	return r, nil
}
func (a *Service) relayRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/relay/transfers", func(w http.ResponseWriter, r *http.Request) { a.relayProxy(w, r, "GET", "/v1/transfers") })
	api.HandleFunc("POST /api/relay/auto-pair", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decode(w, r, &p); err != nil || p.Enabled == nil {
			fail(w, errors.New("必须提供 enabled"))
			return
		}
		if a.role != "agent" {
			fail(w, errors.New("自动配对仅适用于录制机"))
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.active {
			fail(w, errors.New("请等待当前录制和回传结束后调整自动配对"))
			return
		}
		a.s.Config.RelayAutoPair = *p.Enabled
		a.saveLocked()
		if a.storageErr != "" {
			fail(w, errors.New(a.storageErr))
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("GET /api/relay/nodes", func(w http.ResponseWriter, r *http.Request) { a.relayProxy(w, r, "GET", "/v1/nodes") })
	api.HandleFunc("POST /api/relay/pair", func(w http.ResponseWriter, r *http.Request) { a.relayProxy(w, r, "POST", "/v1/pair") })
}
func (a *Service) relayProxy(w http.ResponseWriter, r *http.Request, method, path string) {
	a.mu.Lock()
	c := a.s.Config
	a.mu.Unlock()
	if c.ConnectionMode != "relay" {
		fail(w, errors.New("请先保存云中继配置"))
		return
	}
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if e != nil {
		fail(w, e)
		return
	}
	req, e := relayRequest(r.Context(), c, method, path, bytes.NewReader(b))
	if e != nil {
		fail(w, e)
		return
	}
	res, e := relayControlHTTP.Do(req)
	if e != nil {
		fail(w, errors.New("无法访问云中继"))
		return
	}
	defer res.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.StatusCode)
	io.Copy(w, io.LimitReader(res.Body, 1<<20))
}
func (a *Service) uploadRelayMedia(ctx context.Context, c Config, m relay.Message) {
	if a.role != "agent" || !validID(m.Artifact) || m.Offset < 0 || m.Offset >= m.Size {
		return
	}
	a.mu.Lock()
	var art Artifact
	for _, v := range a.s.Artifacts {
		if v.ID == m.Artifact && v.SHA256 == m.Hash && v.Size == m.Size {
			art = v
			break
		}
	}
	a.mu.Unlock()
	if art.Path == "" {
		return
	}
	f, e := os.Open(art.Path)
	if e != nil {
		return
	}
	defer f.Close()
	c.RelayPair = m.Pair
	offset := m.Offset
	buf := make([]byte, relay.MaxChunk)
	for offset < m.Size && ctx.Err() == nil {
		a.mu.Lock()
		current := a.s.Config
		valid := current.ConnectionMode == "relay" && current.RelayPair == m.Pair && current.RelayGroup == c.RelayGroup && current.RelayURL == c.RelayURL && current.RelayDevice == c.RelayDevice
		a.mu.Unlock()
		if !valid {
			return
		}
		size := min(int64(len(buf)), m.Size-offset)
		n, e := f.ReadAt(buf[:size], offset)
		if e != nil && e != io.EOF {
			return
		}
		if int64(n) != size {
			return
		}
		success := false
		for attempt := 0; attempt < 3; attempt++ {
			req, e := relayRequest(ctx, c, "PUT", "/v1/uploads/"+m.ID+"?offset="+strconv.FormatInt(offset, 10), bytes.NewReader(buf[:n]))
			if e != nil {
				return
			}
			req.Header.Set("Content-Type", "application/octet-stream")
			res, e := relayControlHTTP.Do(req)
			if e != nil {
				continue
			}
			var result struct {
				Offset int64 `json:"offset"`
			}
			e = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&result)
			res.Body.Close()
			if e == nil && (res.StatusCode == 200 || res.StatusCode == 409) && result.Offset >= 0 && result.Offset <= m.Size {
				offset = result.Offset
				success = true
				break
			}
			if res.StatusCode >= 400 && res.StatusCode < 500 {
				return
			}
		}
		if !success {
			return
		}
	}
}
