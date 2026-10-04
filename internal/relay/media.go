package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type transfer struct {
	mu                                              sync.Mutex
	id, artifact, hash, pair, agent, director, path string
	size, offset                                    int64
	ready                                           bool
	created                                         time.Time
	lastRequest                                     time.Time
}

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	artifact := r.PathValue("id")
	hash := r.Header.Get("X-Replay-SHA256")
	size, e := strconv.ParseInt(r.Header.Get("X-Replay-Size"), 10, 64)
	if !IDPattern.MatchString(artifact) || !HashPattern.MatchString(hash) || e != nil || size <= 0 || size > 1<<30 {
		problem(w, 400, "无效素材信息")
		return
	}
	s.mu.Lock()
	n, a, c := s.pairedLocked(r)
	if c == nil {
		s.mu.Unlock()
		problem(w, 409, "配对已变化或节点离线")
		return
	}
	var t *transfer
	var reserved int64
	for _, v := range s.transfers {
		reserved += v.size
		if v.pair == n.Pair && v.artifact == artifact && v.hash == hash && v.size == size {
			t = v
		}
	}
	if t == nil {
		if reserved+size > s.quota {
			s.mu.Unlock()
			problem(w, 507, "云端缓存配额不足")
			return
		}
		id := ID()
		t = &transfer{id: id, artifact: artifact, hash: hash, size: size, pair: n.Pair, agent: a.ID, director: n.ID, path: filepath.Join(s.dir, id+".clip"), created: time.Now()}
		s.transfers[id] = t
	}
	s.mu.Unlock()
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		s.mu.Lock()
		nn, _, cc := s.pairedLocked(r)
		valid := cc != nil && nn.Pair == t.pair && s.transfers[t.id] == t
		s.mu.Unlock()
		if !valid {
			problem(w, 409, "配对失效或缓存已过期")
			return
		}
		t.mu.Lock()
		ready := t.ready
		if !ready && time.Since(t.lastRequest) > 2*time.Second {
			t.lastRequest = time.Now()
			cc.send(Message{Type: "media", ID: t.id, Pair: t.pair, Artifact: t.artifact, Hash: t.hash, Size: t.size, Offset: t.offset, Deadline: time.Now().Add(5 * time.Minute).UnixMilli()})
		}
		t.mu.Unlock()
		if ready {
			f, err := os.Open(t.path)
			if err != nil {
				problem(w, 503, "缓存不可读，请重试")
				return
			}
			defer f.Close()
			w.Header().Set("ETag", `"`+t.hash+`"`)
			w.Header().Set("X-Replay-Transfer", t.id)
			w.Header().Set("Content-Type", "video/mp4")
			http.ServeContent(&pairWriter{ResponseWriter: w, server: s, request: r, pair: t.pair}, r, t.artifact+".mp4", t.created, f)
			return
		}
		select {
		case <-poll.C:
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-deadline.C:
			problem(w, 504, "等待节点上传超时，可重试续传")
			return
		}
	}
}
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxChunk))
	if e != nil || len(b) == 0 {
		problem(w, 400, "无效上传块")
		return
	}
	s.mu.Lock()
	t := s.transfers[r.PathValue("id")]
	n := s.nodeLocked(r)
	if t == nil || n == nil || n.Role != "agent" || n.ID != t.agent || n.Pair != t.pair || r.Header.Get("X-Replay-Pair") != t.pair {
		s.mu.Unlock()
		problem(w, 403, "上传不属于当前配对")
		return
	}
	if !t.mu.TryLock() {
		s.mu.Unlock()
		problem(w, 503, "上传忙，请重试")
		return
	}
	s.mu.Unlock()
	defer t.mu.Unlock()
	offset, e := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if e != nil || offset != t.offset {
		reply(w, 409, map[string]any{"error": "上传偏移变化", "offset": t.offset})
		return
	}
	if t.offset+int64(len(b)) > t.size {
		problem(w, 400, "无效上传块")
		return
	}
	f, e := os.OpenFile(t.path, os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		problem(w, 507, "无法写入云端缓存")
		return
	}
	start := t.offset
	nbytes, e := f.WriteAt(b, start)
	if e == nil {
		e = f.Sync()
	}
	if e != nil || nbytes != len(b) {
		f.Truncate(start)
		f.Close()
		problem(w, 507, "云端缓存写入失败")
		return
	}
	f.Close()
	t.offset += int64(nbytes)
	if t.offset == t.size {
		f, e := os.Open(t.path)
		if e != nil {
			problem(w, 507, "缓存校验读取失败")
			return
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		f.Close()
		if e != nil || hex.EncodeToString(h.Sum(nil)) != t.hash {
			os.Remove(t.path)
			t.offset = 0
			problem(w, 422, "SHA-256 校验失败，需重新上传")
			return
		}
		t.ready = true
	}
	reply(w, 200, map[string]any{"offset": t.offset, "ready": t.ready})
}
func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, _, c := s.pairedLocked(r)
	t := s.transfers[r.PathValue("id")]
	if c == nil || t == nil || t.director != n.ID || t.pair != n.Pair {
		problem(w, 404, "未找到当前配对缓存")
		return
	}
	if !t.mu.TryLock() {
		problem(w, 409, "上传处理中")
		return
	}
	defer t.mu.Unlock()
	if !t.ready {
		problem(w, 409, "缓存尚未完成")
		return
	}
	if err := os.Remove(t.path); err != nil && !os.IsNotExist(err) {
		problem(w, 500, "缓存删除失败")
		return
	}
	delete(s.transfers, t.id)
	reply(w, 200, map[string]bool{"ok": true})
}
func (s *Server) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.transfers {
		if !t.mu.TryLock() {
			continue
		}
		n := s.nodes[t.director]
		if time.Since(t.created) > 24*time.Hour || n == nil || n.Pair != t.pair {
			if e := os.Remove(t.path); e == nil || os.IsNotExist(e) {
				delete(s.transfers, id)
			}
		}
		t.mu.Unlock()
	}
}

// CacheUsage is available to deployment checks without exposing device identities.
func (s *Server) CacheUsage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, t := range s.transfers {
		total += t.size
	}
	return fmt.Sprintf("%d/%d", total, s.quota)
}

// Check authorization between streaming writes as well as at request admission.
type pairWriter struct {
	http.ResponseWriter
	server  *Server
	request *http.Request
	pair    string
}

func (w *pairWriter) Write(b []byte) (int, error) {
	w.server.mu.Lock()
	n, _, c := w.server.pairedLocked(w.request)
	valid := c != nil && n.Pair == w.pair
	w.server.mu.Unlock()
	if !valid {
		return 0, errors.New("pair revoked during download")
	}
	return w.ResponseWriter.Write(b)
}

func (s *Server) transferStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodeLocked(r)
	if n == nil {
		problem(w, 409, "设备离线或群组已变化")
		return
	}
	result := []map[string]any{}
	for _, t := range s.transfers {
		if t.pair != n.Pair || n.Pair == "" || !t.mu.TryLock() {
			continue
		}
		result = append(result, map[string]any{"artifact": t.artifact, "uploaded": t.offset, "size": t.size, "ready": t.ready})
		t.mu.Unlock()
	}
	reply(w, 200, result)
}
