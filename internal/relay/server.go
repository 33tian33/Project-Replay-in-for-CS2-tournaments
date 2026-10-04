package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type connection struct {
	ws   *websocket.Conn
	out  chan Message
	done chan struct{}
	once sync.Once
}

func (c *connection) close() { c.once.Do(func() { close(c.done); c.ws.Close() }) }
func (c *connection) send(m Message) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.out <- m:
		return true
	default:
		c.close()
		return false
	}
}

type pending struct {
	agent, director, pair string
	response              chan Message
}
type Server struct {
	mu          sync.Mutex
	nodes       map[string]*Node
	connections map[string]*connection
	pending     map[string]*pending
	changed     map[string]time.Time
	actions     map[string][]time.Time
	transfers   map[string]*transfer
	dir         string
	quota       int64
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	storageErr  error
}

func NewServer(dir string, quota int64) (*Server, error) {
	if quota < 1<<30 {
		return nil, errors.New("cache quota must be at least 1 GiB")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{nodes: map[string]*Node{}, connections: map[string]*connection{}, pending: map[string]*pending{}, changed: map[string]time.Time{}, actions: map[string][]time.Time{}, transfers: map[string]*transfer{}, dir: dir, quota: quota, ctx: ctx, cancel: cancel}
	raw, err := os.ReadFile(filepath.Join(dir, "nodes.json"))
	if err == nil {
		if err = json.Unmarshal(raw, &s.nodes); err != nil {
			cancel()
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		cancel()
		return nil, err
	}
	for id, n := range s.nodes {
		if n == nil || !IDPattern.MatchString(id) || n.ID != id || !GroupPattern.MatchString(n.Group) || (n.Role != "director" && n.Role != "agent") {
			delete(s.nodes, id)
			continue
		}
		n.Online = false
		n.Pending = ""
	}
	for _, n := range s.nodes {
		p := s.nodes[n.Peer]
		if p == nil || p.Peer != n.ID || p.Group != n.Group || p.Pair != n.Pair || p.Role == n.Role {
			n.Peer = ""
			n.Pair = ""
		}
	}
	// Cache is ephemeral. Restart invalidates upload tickets; clients retry from their local files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".clip") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				cancel()
				return nil, err
			}
		}
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.expire()
			}
		}
	}()
	return s, nil
}
func (s *Server) Close() {
	s.cancel()
	s.mu.Lock()
	for _, c := range s.connections {
		c.close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
func (s *Server) saveLocked() {
	b, e := json.Marshal(s.nodes)
	if e == nil {
		e = os.WriteFile(filepath.Join(s.dir, "nodes.tmp"), b, 0600)
	}
	if e == nil {
		e = os.Rename(filepath.Join(s.dir, "nodes.tmp"), filepath.Join(s.dir, "nodes.json"))
	}
	s.storageErr = e
}
func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, code int, msg string) {
	reply(w, code, map[string]string{"error": msg})
}
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxChunk)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /v1/connect", s.connect)
	mux.HandleFunc("GET /v1/nodes", s.discover)
	mux.HandleFunc("GET /v1/transfers", s.transferStatus)
	mux.HandleFunc("POST /v1/pair", s.pairAction)
	mux.HandleFunc("/v1/forward/", s.forward)
	mux.HandleFunc("GET /v1/media/{id}", s.media)
	mux.HandleFunc("PUT /v1/uploads/{id}", s.upload)
	mux.HandleFunc("POST /v1/media-ack/{id}", s.ack)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/healthz" {
			if !IDPattern.MatchString(r.Header.Get("X-Replay-Device")) || !GroupPattern.MatchString(r.Header.Get("X-Replay-Group")) {
				problem(w, 400, "设备标识或群组码无效")
				return
			}
			if r.Header.Get("Origin") != "" {
				problem(w, 403, "中继接口仅供设备客户端使用")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) nodeLocked(r *http.Request) *Node {
	n := s.nodes[r.Header.Get("X-Replay-Device")]
	if n == nil || !n.Online || n.Group != r.Header.Get("X-Replay-Group") {
		return nil
	}
	return n
}
func (s *Server) publishLocked(n *Node) {
	if n != nil {
		if c := s.connections[n.ID]; c != nil {
			copy := *n
			if peer := s.nodes[n.Peer]; peer != nil {
				copy.PeerOnline = peer.Online
			}
			c.send(Message{Type: "state", Node: &copy})
		}
	}
}
func (s *Server) unpairLocked(n *Node) {
	p := s.nodes[n.Peer]
	old := n.Pair
	n.Peer = ""
	n.Pair = ""
	n.Pending = ""
	if p != nil {
		p.Peer = ""
		p.Pair = ""
		p.Pending = ""
		s.publishLocked(p)
	}
	for _, other := range s.nodes {
		if other.Pending == n.ID {
			other.Pending = ""
			s.publishLocked(other)
		}
	}
	for _, req := range s.pending {
		if req.pair == old {
			select {
			case req.response <- Message{Status: 409, Body: []byte(`{"error":"配对已解除"}`)}:
			default:
			}
		}
	}
	s.publishLocked(n)
}
func (s *Server) bindLocked(a, b *Node) {
	a.Peer = b.ID
	b.Peer = a.ID
	a.Pair = ID()
	b.Pair = a.Pair
	a.Pending = ""
	b.Pending = ""
	for _, n := range s.nodes {
		if n.Pending == a.ID || n.Pending == b.ID {
			n.Pending = ""
			s.publishLocked(n)
		}
	}
	s.saveLocked()
	s.publishLocked(a)
	s.publishLocked(b)
}
func (s *Server) automaticLocked() {
	ids := make([]string, 0, len(s.nodes))
	for id := range s.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, di := range ids {
		d := s.nodes[di]
		if d.Role != "director" || !d.Online || d.Peer != "" {
			continue
		}
		for _, ai := range ids {
			a := s.nodes[ai]
			if a.Role == "agent" && a.Auto && a.Online && a.Peer == "" && a.Group == d.Group {
				s.bindLocked(d, a)
				break
			}
		}
	}
}
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	device := r.Header.Get("X-Replay-Device")
	up := websocket.Upgrader{HandshakeTimeout: 5 * time.Second}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(12 << 20)
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var h Hello
	if err = ws.ReadJSON(&h); err != nil || !GroupPattern.MatchString(h.Group) || len(h.Name) > 80 || (h.Role != "director" && h.Role != "agent") || h.Group != r.Header.Get("X-Replay-Group") {
		return
	}
	c := &connection{ws: ws, out: make(chan Message, 64), done: make(chan struct{})}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	defer s.wg.Done()
	n := s.nodes[device]
	if n != nil && n.Role != h.Role {
		s.mu.Unlock()
		return
	}
	if n != nil && n.Group != h.Group && time.Since(s.changed[n.ID]) < 5*time.Second {
		s.mu.Unlock()
		return
	}
	if old := s.connections[device]; old != nil {
		old.close()
	}
	if n == nil {
		n = &Node{ID: device, Role: h.Role}
		s.nodes[device] = n
	}
	if n.Group != h.Group {
		s.unpairLocked(n)
		s.changed[n.ID] = time.Now()
	}
	n.Group = h.Group
	n.Name = h.Name
	n.Auto = h.Auto && h.Role == "agent"
	n.Online = true
	s.connections[device] = c
	s.publishLocked(n)
	s.publishLocked(s.nodes[n.Peer])
	s.automaticLocked()
	s.saveLocked()
	s.mu.Unlock()
	defer func() {
		c.close()
		s.mu.Lock()
		if s.connections[device] == c {
			delete(s.connections, device)
			n.Online = false
			s.publishLocked(s.nodes[n.Peer])
			s.saveLocked()
		}
		s.mu.Unlock()
	}()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case m := <-c.out:
				ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if ws.WriteJSON(m) != nil {
					c.close()
					return
				}
			case <-t.C:
				if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					c.close()
					return
				}
			}
		}
	}()
	defer func() { c.close(); <-writerDone }()
	ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	for {
		var m Message
		if ws.ReadJSON(&m) != nil {
			return
		}
		if m.Type != "response" {
			continue
		}
		s.mu.Lock()
		p := s.pending[m.ID]
		if p != nil && p.agent == device && n.Pair == p.pair && m.Pair == p.pair && s.connections[device] == c {
			select {
			case p.response <- m:
			default:
			}
		}
		s.mu.Unlock()
	}
}
func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodeLocked(r)
	if n == nil {
		problem(w, 409, "设备未连接或群组已变化")
		return
	}
	nodes := []Node{}
	for _, other := range s.nodes {
		if other.ID != n.ID && other.Group == n.Group && other.Role != n.Role && other.Online {
			v := *other
			v.Pair = ""
			v.Pending = ""
			nodes = append(nodes, v)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	reply(w, 200, map[string]any{"self": n, "nodes": nodes})
}
func (s *Server) pairAction(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Action string `json:"action"`
		Target string `json:"target"`
	}
	if readJSON(w, r, &p) != nil {
		problem(w, 400, "无效配对请求")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodeLocked(r)
	if n == nil {
		problem(w, 409, "设备未连接或群组已变化")
		return
	}
	recent := s.actions[n.ID][:0]
	for _, t := range s.actions[n.ID] {
		if time.Since(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	s.actions[n.ID] = recent
	if len(recent) >= 30 {
		problem(w, 429, "配对操作过于频繁")
		return
	}
	s.actions[n.ID] = append(recent, time.Now())
	if p.Action == "unpair" {
		s.unpairLocked(n)
		s.saveLocked()
		reply(w, 200, n)
		return
	}
	other := s.nodes[p.Target]
	if other == nil || other.Group != n.Group || !other.Online || other.Role == n.Role {
		problem(w, 404, "未发现可配对设备")
		return
	}
	if n.Peer != "" || other.Peer != "" {
		problem(w, 409, "设备已有配对，请先解除")
		return
	}
	switch p.Action {
	case "request":
		if n.Role != "director" {
			problem(w, 403, "仅主机可发起配对")
			return
		}
		if other.Auto {
			s.bindLocked(n, other)
		} else {
			if other.Pending != "" && other.Pending != n.ID {
				problem(w, 409, "录制机正在等待其他配对确认")
				return
			}
			other.Pending = n.ID
			s.publishLocked(other)
		}
	case "accept", "reject":
		if n.Role != "agent" || n.Pending != other.ID {
			problem(w, 409, "没有对应配对请求")
			return
		}
		if p.Action == "accept" {
			s.bindLocked(n, other)
		} else {
			n.Pending = ""
			s.publishLocked(n)
		}
	default:
		problem(w, 400, "未知配对操作")
		return
	}
	if s.storageErr != nil {
		problem(w, 503, "配对状态持久化失败")
		return
	}
	reply(w, 200, n)
}
func (s *Server) pairedLocked(r *http.Request) (*Node, *Node, *connection) {
	if s.storageErr != nil {
		return nil, nil, nil
	}
	n := s.nodeLocked(r)
	if n == nil || n.Role != "director" || n.Pair == "" || r.Header.Get("X-Replay-Pair") != n.Pair {
		return nil, nil, nil
	}
	p := s.nodes[n.Peer]
	if p == nil || p.Group != n.Group || p.Pair != n.Pair || p.Peer != n.ID || !p.Online {
		return nil, nil, nil
	}
	return n, p, s.connections[p.ID]
}
func (s *Server) forward(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/forward")
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	if !Allowed(r.Method, path) {
		problem(w, 403, "不允许转发此接口")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxChunk))
	if err != nil {
		problem(w, 413, "指令过大")
		return
	}
	s.mu.Lock()
	n, a, c := s.pairedLocked(r)
	if c == nil {
		s.mu.Unlock()
		problem(w, 409, "尚未配对、配对已变化或节点离线")
		return
	}
	if len(s.pending) >= 256 {
		s.mu.Unlock()
		problem(w, 429, "中继繁忙")
		return
	}
	p := &pending{agent: a.ID, director: n.ID, pair: n.Pair, response: make(chan Message, 1)}
	id := ID()
	s.pending[id] = p
	d := s.connections[n.ID]
	c.send(Message{Type: "request", ID: id, Pair: p.pair, Method: r.Method, Path: path, Body: body, Deadline: time.Now().Add(12 * time.Second).UnixMilli()})
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case m := <-p.response:
		if m.Status < 200 || m.Status > 599 {
			problem(w, 502, "无效节点响应")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(m.Status)
		w.Write(m.Body)
	case <-c.done:
		problem(w, 503, "录制机已断开；请按原任务 ID 核对结果")
	case <-d.done:
		problem(w, 503, "主机已断开")
	case <-timer.C:
		problem(w, 504, "节点响应超时；请按原任务 ID 核对结果")
	case <-r.Context().Done():
	case <-s.ctx.Done():
	}
}
