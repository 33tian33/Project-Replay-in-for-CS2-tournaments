package relay

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// NormalizeURL accepts a bare IP with the relay's default port, or IP:port.
// Explicit HTTP/HTTPS URLs keep their standard port semantics.
func NormalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if ip := net.ParseIP(strings.Trim(raw, "[]")); ip != nil {
		return "http://" + net.JoinHostPort(ip.String(), "7790")
	}
	if host, port, err := net.SplitHostPort(raw); err == nil && net.ParseIP(host) != nil {
		return "http://" + net.JoinHostPort(host, port)
	}
	return strings.TrimRight(raw, "/")
}

func ValidateURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("中继地址须为云服务器 IP、IP:端口或 http(s)://地址:端口，不含路径或凭据")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("中继端口须为 1–65535")
		}
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()) != nil) {
		return nil
	}
	return errors.New("IP 中继支持 HTTP；域名中继请使用 HTTPS")
}
func Headers(r *http.Request, id, group, pair string) {
	r.Header.Set("X-Replay-Device", id)
	r.Header.Set("X-Replay-Group", group)
	if pair != "" {
		r.Header.Set("X-Replay-Pair", pair)
	}
}

// Run owns all work for one connection. Cancelling it closes the socket and
// cancels in-flight local handlers; commands are never buffered for replay.
func Run(ctx context.Context, base, id string, h Hello, state func(Node), handle func(context.Context, Message) Message, media func(context.Context, Message)) error {
	if err := ValidateURL(base); err != nil {
		return err
	}
	u, _ := url.Parse(strings.TrimRight(base, "/") + "/v1/connect")
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	headers := http.Header{}
	headers.Set("X-Replay-Device", id)
	headers.Set("X-Replay-Group", h.Group)
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}
	ws, res, e := d.DialContext(ctx, u.String(), headers)
	if e != nil {
		if res != nil {
			res.Body.Close()
			return errors.New("中继握手失败：" + res.Status)
		}
		return errors.New("无法连接云中继，请检查地址、网络和证书")
	}
	defer ws.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			ws.Close()
		case <-done:
		}
	}()
	defer close(done)
	ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if e = ws.WriteJSON(h); e != nil {
		return e
	}
	ws.SetReadLimit(2 << 20)
	ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	ws.SetPingHandler(func(data string) error {
		ws.SetReadDeadline(time.Now().Add(45 * time.Second))
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	var writes sync.Mutex
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	send := func(m Message) {
		writes.Lock()
		defer writes.Unlock()
		ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if ws.WriteJSON(m) != nil {
			cancel()
		}
	}
	var pair string
	pairCtx, pairCancel := context.WithCancel(ctx)
	defer func() { pairCancel() }()
	slots := make(chan struct{}, 16)
	uploads := make(chan struct{}, 1)
	for {
		var m Message
		if e = ws.ReadJSON(&m); e != nil {
			pairCancel()
			return e
		}
		if m.Type == "state" && m.Node != nil {
			if m.Node.Pair != pair {
				pairCancel()
				pairCtx, pairCancel = context.WithCancel(ctx)
				pair = m.Node.Pair
			}
			state(*m.Node)
			continue
		}
		if m.Type != "request" && m.Type != "media" {
			continue
		}
		if pair == "" || pair != m.Pair || m.Deadline <= time.Now().UnixMilli() {
			if m.Type == "request" {
				send(Message{Type: "response", ID: m.ID, Pair: m.Pair, Status: 409, Body: []byte(`{"error":"配对或指令已过期"}`)})
			}
			continue
		}
		limit := slots
		if m.Type == "media" {
			limit = uploads
		}
		select {
		case limit <- struct{}{}:
		default:
			if m.Type == "request" {
				send(Message{Type: "response", ID: m.ID, Pair: m.Pair, Status: 429, Body: []byte(`{"error":"节点繁忙"}`)})
			}
			continue
		}
		workCtx := pairCtx
		workers.Add(1)
		go func(m Message) {
			defer workers.Done()
			defer func() { <-limit }()
			deadline := time.UnixMilli(m.Deadline)
			maxDeadline := time.Now().Add(15 * time.Second)
			if m.Type == "media" {
				maxDeadline = time.Now().Add(5 * time.Minute)
			}
			if deadline.After(maxDeadline) {
				deadline = maxDeadline
			}
			callCtx, stop := context.WithDeadline(workCtx, deadline)
			defer stop()
			if m.Type == "media" {
				media(callCtx, m)
				return
			}
			result := handle(callCtx, m)
			result.Type = "response"
			result.ID = m.ID
			result.Pair = m.Pair
			send(result)
		}(m)
	}
}
