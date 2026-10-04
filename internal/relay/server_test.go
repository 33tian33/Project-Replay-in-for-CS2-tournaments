package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type testDevice struct{ ID, Role string }

type testPeer struct {
	credential testDevice
	group      string
	mu         sync.Mutex
	node       Node
	cancel     context.CancelFunc
	done       chan struct{}
}

func (p *testPeer) snapshot() Node { p.mu.Lock(); defer p.mu.Unlock(); return p.node }
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for relay state")
}
func newTestRelay(t *testing.T) (*Server, *httptest.Server, []testDevice) {
	t.Helper()
	cs := []testDevice{{"d1", "director"}, {"d2", "director"}, {"a1", "agent"}, {"a2", "agent"}}
	s, e := NewServer(t.TempDir(), 1<<30)
	if e != nil {
		t.Fatal(e)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { s.Close(); h.Close() })
	return s, h, cs
}
func startPeer(t *testing.T, h *httptest.Server, c testDevice, group string, auto bool, media func(context.Context, Message)) *testPeer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &testPeer{credential: c, group: group, cancel: cancel, done: make(chan struct{})}
	if media == nil {
		media = func(context.Context, Message) {}
	}
	go func() {
		defer close(p.done)
		Run(ctx, h.URL, c.ID, Hello{Role: c.Role, Group: group, Auto: auto}, func(n Node) { p.mu.Lock(); p.node = n; p.mu.Unlock() }, func(ctx context.Context, m Message) Message { return Message{Status: 200, Body: []byte(`{"ok":true}`)} }, media)
	}()
	t.Cleanup(func() { cancel(); <-p.done })
	eventually(t, func() bool { return p.snapshot().Online })
	return p
}
func callPeer(t *testing.T, h *httptest.Server, p *testPeer, method, path string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, h.URL+path, bytes.NewReader(b))
	Headers(req, p.credential.ID, p.group, p.snapshot().Pair)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, data
}
func TestGroupIsolationManualAutoAndNoStealing(t *testing.T) {
	s, h, cs := newTestRelay(t)
	d := startPeer(t, h, cs[0], "0001", false, nil)
	other := startPeer(t, h, cs[1], "0002", false, nil)
	a := startPeer(t, h, cs[2], "0001", false, nil)
	code, b := callPeer(t, h, other, "GET", "/v1/nodes", nil)
	if code != 200 || bytes.Contains(b, []byte(`"id":"a1"`)) {
		t.Fatalf("cross-group discovery: %d %s", code, b)
	}
	if code, _ = callPeer(t, h, other, "POST", "/v1/pair", map[string]string{"action": "request", "target": "a1"}); code != 404 {
		t.Fatal(code)
	}
	if code, _ = callPeer(t, h, d, "POST", "/v1/pair", map[string]string{"action": "request", "target": "a1"}); code != 200 {
		t.Fatal(code)
	}
	eventually(t, func() bool { return a.snapshot().Pending == "d1" })
	if a.snapshot().Peer != "" {
		t.Fatal("manual mode paired without confirmation")
	}
	callPeer(t, h, a, "POST", "/v1/pair", map[string]string{"action": "accept", "target": "d1"})
	eventually(t, func() bool { return d.snapshot().Peer == "a1" })
	oldPair := d.snapshot().Pair
	code, _ = callPeer(t, h, d, "POST", "/v1/forward/api/connect-gotv?agent=1", map[string]string{"gotv": "example:27020"})
	if code != 200 {
		t.Fatal(code)
	}
	code, _ = callPeer(t, h, d, "POST", "/v1/forward/api/config", map[string]string{})
	if code != 403 {
		t.Fatal("unsafe API allowed", code)
	}
	// A forged same-group header is checked against authenticated server state.
	req, _ := http.NewRequest("GET", h.URL+"/v1/nodes", nil)
	Headers(req, cs[1].ID, "0001", oldPair)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 409 {
		t.Fatal(res.StatusCode)
	}
	callPeer(t, h, d, "POST", "/v1/pair", map[string]string{"action": "unpair"})
	eventually(t, func() bool { return a.snapshot().Peer == "" && d.snapshot().Peer == "" })
	a.cancel()
	<-a.done
	a = startPeer(t, h, cs[2], "0001", true, nil)
	eventually(t, func() bool { return d.snapshot().Peer == "a1" })
	if d.snapshot().Pair == oldPair {
		t.Fatal("pair generation reused")
	}
	s.mu.Lock()
	s.changed[cs[1].ID] = time.Time{}
	s.mu.Unlock()
	other.cancel()
	<-other.done
	other = startPeer(t, h, cs[1], "0001", false, nil)
	if code, _ = callPeer(t, h, other, "POST", "/v1/pair", map[string]string{"action": "request", "target": "a1"}); code != 409 {
		t.Fatal("stole pair", code)
	}
	req, _ = http.NewRequest("GET", h.URL+"/v1/forward/api/time", nil)
	Headers(req, cs[0].ID, "0001", oldPair)
	res, e = http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 409 {
		t.Fatal("stale pair allowed", res.StatusCode)
	}
	// Changing group invalidates both ends immediately at the next registration.
	s.mu.Lock()
	s.changed[cs[2].ID] = time.Time{}
	s.mu.Unlock()
	a.cancel()
	<-a.done
	startPeer(t, h, cs[2], "0003", true, nil)
	eventually(t, func() bool { return d.snapshot().Peer == "" })
}
func TestRelayMediaResumableHashRangeAndAck(t *testing.T) {
	s, h, cs := newTestRelay(t)
	payload := bytes.Repeat([]byte("recorded-video"), 180000)
	hash := sha256.Sum256(payload)
	digest := hex.EncodeToString(hash[:])
	d := startPeer(t, h, cs[0], "0099", false, nil)
	uploads := make(chan Message, 8)
	a := startPeer(t, h, cs[2], "0099", true, func(ctx context.Context, m Message) { uploads <- m })
	eventually(t, func() bool { return d.snapshot().Pair != "" })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.URL+"/v1/media/clip1", nil)
	Headers(req, cs[0].ID, "0099", d.snapshot().Pair)
	req.Header.Set("X-Replay-SHA256", digest)
	req.Header.Set("X-Replay-Size", jsonNumber(len(payload)))
	req.Header.Set("Range", "bytes=17-")
	result := make(chan *http.Response, 1)
	errs := make(chan error, 1)
	go func() {
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			errs <- e
		} else {
			result <- res
		}
	}()
	var m Message
	select {
	case m = <-uploads:
	case <-time.After(3 * time.Second):
		t.Fatal("missing upload request")
	}
	upload := func(offset int, b []byte, want int) {
		t.Helper()
		r, _ := http.NewRequest("PUT", h.URL+"/v1/uploads/"+m.ID+"?offset="+jsonNumber(offset), bytes.NewReader(b))
		Headers(r, cs[2].ID, a.group, a.snapshot().Pair)
		res, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("upload status %d wanted %d", res.StatusCode, want)
		}
	}
	upload(0, payload[:MaxChunk], 200)
	upload(0, payload[:MaxChunk], 409)
	// Control traffic remains available while an incomplete upload waits.
	if code, _ := callPeer(t, h, d, "GET", "/v1/forward/api/time", nil); code != 200 {
		t.Fatal(code)
	}
	for offset := MaxChunk; offset < len(payload); {
		end := min(offset+MaxChunk, len(payload))
		upload(offset, payload[offset:end], 200)
		offset = end
	}
	select {
	case res := <-result:
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 206 || !bytes.Equal(b, payload[17:]) {
			t.Fatal("range download mismatch", res.StatusCode)
		}
	case e := <-errs:
		t.Fatal(e)
	case <-time.After(3 * time.Second):
		t.Fatal("download never completed")
	}
	if code, _ := callPeer(t, h, d, "POST", "/v1/media-ack/"+m.ID, nil); code != 200 {
		t.Fatal(code)
	}
	s.mu.Lock()
	count := len(s.transfers)
	s.mu.Unlock()
	if count != 0 {
		t.Fatal("cache not removed after ack")
	}
}
func jsonNumber(n int) string { b, _ := json.Marshal(n); return string(b) }
func TestGroupValidationAndPersistence(t *testing.T) {
	dir := t.TempDir()
	creds := []testDevice{{"d", "director"}, {"a", "agent"}}
	s, e := NewServer(dir, 1<<30)
	if e != nil {
		t.Fatal(e)
	}
	h := httptest.NewServer(s.Handler())
	d := startPeer(t, h, creds[0], "0000", false, nil)
	a := startPeer(t, h, creds[1], "0000", true, nil)
	eventually(t, func() bool { return d.snapshot().Pair != "" })
	pair := d.snapshot().Pair
	req, _ := http.NewRequest("GET", h.URL+"/v1/nodes", nil)
	Headers(req, "d", "bad", pair)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatal(res.StatusCode)
	}
	d.cancel()
	a.cancel()
	<-d.done
	<-a.done
	s.Close()
	h.Close()
	s, e = NewServer(dir, 1<<30)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if s.nodes["d"].Pair != pair || s.nodes["d"].Online {
		t.Fatal("pair persistence broken")
	}
	for _, g := range []string{"123", "12345", "１２３４", "12a4", " 123"} {
		if GroupPattern.MatchString(g) {
			t.Fatal(g)
		}
	}
	for _, u := range []string{"http://public.example", "https://u:p@example.com", "https://example.com/path"} {
		if ValidateURL(u) == nil {
			t.Fatal(u)
		}
	}
}

func TestConcurrentPairRequestsAndRevokedMedia(t *testing.T) {
	s, h, cs := newTestRelay(t)
	d1 := startPeer(t, h, cs[0], "4321", false, nil)
	d2 := startPeer(t, h, cs[1], "4321", false, nil)
	a := startPeer(t, h, cs[2], "4321", false, nil)
	statuses := make(chan int, 2)
	for _, d := range []*testPeer{d1, d2} {
		go func(d *testPeer) {
			code, _ := callPeer(t, h, d, "POST", "/v1/pair", map[string]string{"action": "request", "target": "a1"})
			statuses <- code
		}(d)
	}
	codes := []int{<-statuses, <-statuses}
	if !(codes[0] == 200 && codes[1] == 409 || codes[0] == 409 && codes[1] == 200) {
		t.Fatal("concurrent requests did not serialize", codes)
	}
	eventually(t, func() bool { return a.snapshot().Pending != "" })
	winner := d1
	if a.snapshot().Pending == "d2" {
		winner = d2
	}
	callPeer(t, h, a, "POST", "/v1/pair", map[string]string{"action": "accept", "target": winner.credential.ID})
	eventually(t, func() bool { return winner.snapshot().Pair != "" })
	payload := []byte("complete-video")
	sum := sha256.Sum256(payload)
	id := ID()
	tr := &transfer{id: id, artifact: "integrity", hash: hex.EncodeToString(sum[:]), size: int64(len(payload)), pair: winner.snapshot().Pair, agent: cs[2].ID, director: winner.credential.ID, path: filepath.Join(s.dir, id+".clip"), created: time.Now()}
	s.mu.Lock()
	s.transfers[id] = tr
	s.mu.Unlock()
	send := func(data []byte, code int) {
		t.Helper()
		req, _ := http.NewRequest("PUT", h.URL+"/v1/uploads/"+id+"?offset=0", bytes.NewReader(data))
		Headers(req, cs[2].ID, "4321", tr.pair)
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != code {
			t.Fatal(res.StatusCode, code)
		}
	}
	send(bytes.Repeat([]byte("x"), len(payload)), 422)
	tr.mu.Lock()
	if tr.ready || tr.offset != 0 {
		t.Fatal("bad hash marked complete")
	}
	tr.mu.Unlock()
	send(payload, 200)
	callPeer(t, h, winner, "POST", "/v1/pair", map[string]string{"action": "unpair"})
	send(payload, 403)
	s.expire()
	s.mu.Lock()
	_, exists := s.transfers[id]
	s.mu.Unlock()
	if exists {
		t.Fatal("revoked-pair cache retained")
	}
}
