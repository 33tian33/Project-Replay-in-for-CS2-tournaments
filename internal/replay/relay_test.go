package replay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"projectreplay/internal/relay"
	"strings"
	"testing"
	"time"
)

func waitRelay(t *testing.T, p func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if p() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("relay condition timed out")
}
func TestRelayIntegrationPreservesLANAndDownloads(t *testing.T) {
	broker, e := relay.NewServer(t.TempDir(), 1<<30)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(broker.Handler())
	defer server.Close()
	defer broker.Close()
	a := testService(t, "agent")
	d := testService(t, "director")
	for i, s := range []*Service{d, a} {
		c := s.s.Config
		c.ConnectionMode = "relay"
		c.RelayURL = strings.TrimPrefix(server.URL, "http://")
		c.RelayGroup = "0007"
		c.RelayAutoPair = i == 1
		c.WorkerURL = "http://192.0.2.1:7788"
		if e = s.configure(c); e != nil {
			t.Fatal(e)
		}
		s.Start()
	}
	defer a.Close()
	defer d.Close()
	waitRelay(t, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.s.Config.RelayPair != "" })
	d.mu.Lock()
	c := d.s.Config
	d.mu.Unlock()
	if e = d.syncRemote(); e != nil {
		t.Fatal(e)
	}
	var result map[string]any
	if e = remoteCall(context.Background(), c, "GET", "/api/time", nil, &result); e != nil || result["now"] == nil {
		t.Fatal(e, result)
	}
	data := bytes.Repeat([]byte("media-data"), 200000)
	hash := sha256.Sum256(data)
	path := filepath.Join(a.dir, "media", "testclip.mp4")
	if e = os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	art := Artifact{ID: "testclip", Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:]), Path: path}
	a.mu.Lock()
	a.s.Artifacts = append(a.s.Artifacts, art)
	a.mu.Unlock()
	if e = d.download(c, &art); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(art.Path)
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal("download content mismatch", e)
	}
	snap, _ := json.Marshal(d.snapshot())
	if bytes.Contains(snap, []byte("relay_token")) {
		t.Fatal("credential leaked in snapshot")
	}
	// The saved LAN address survives opting in and out, and routes exactly as before.
	c.ConnectionMode = "lan"
	u, e := remoteURL(c, "/api/time")
	if e != nil || u != "http://192.0.2.1:7788/api/time" {
		t.Fatal(u, e)
	}
	if e = d.configure(c); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	if d.s.Config.WorkerURL != c.WorkerURL || d.s.Config.RelayPair != "" {
		t.Fatal("LAN configuration changed")
	}
	d.mu.Unlock()
}
func TestRelayConfigValidationAndAutomaticIdentity(t *testing.T) {
	a := testService(t, "agent")
	c := a.s.Config
	c.ConnectionMode = "relay"
	c.RelayURL = "https://relay.example.com"
	c.RelayGroup = "0001"
	c.RelayAutoPair = true
	if e := a.configure(c); e != nil {
		t.Fatal(e)
	}
	c = a.s.Config
	device := c.RelayDevice
	c.RelayDevice = ""
	if e := a.configure(c); e != nil {
		t.Fatal(e)
	}
	if device == "" || a.s.Config.RelayDevice != device {
		t.Fatal("automatic device identity lost")
	}
	c = a.s.Config
	c.RelayGroup = "123"
	if e := a.configure(c); e == nil {
		t.Fatal("invalid group accepted")
	}
	c = a.s.Config
	c.RelayPair = "forged"
	if e := a.configure(c); e != nil {
		t.Fatal(e)
	}
	if a.s.Config.RelayPair == "forged" {
		t.Fatal("client forged pair generation")
	}
	a.Close()
	b, e := New(a.dir, "agent")
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if !b.s.Config.RelayAutoPair || b.s.Config.RelayGroup != "0001" {
		t.Fatal("relay preferences not persisted")
	}
	r := a.relayCommand(context.Background(), relay.Message{Method: "POST", Path: "/api/config"})
	if r.Status != http.StatusForbidden {
		t.Fatal("unsafe relay API accepted")
	}
}

func TestRelayLocalGroupChangeRejectsOldConnectionImmediately(t *testing.T) {
	a := testService(t, "agent")
	c := a.s.Config
	c.ConnectionMode = "relay"
	c.RelayURL = "https://relay.example.com"
	c.RelayGroup = "0001"
	if err := a.configure(c); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.s.Config.RelayPair = "old-pair"
	a.relayState = relayStatus{Online: true, Peer: "director"}
	c = a.s.Config
	a.mu.Unlock()
	c.RelayGroup = "0002"
	if err := a.configure(c); err != nil {
		t.Fatal(err)
	}
	if a.relayState.Online || a.relayState.Peer != "" {
		t.Fatal("old group still displayed as paired")
	}
	res := a.relayCommand(context.Background(), relay.Message{Method: "GET", Path: "/api/time", Pair: "old-pair"})
	if res.Status != 409 {
		t.Fatal("old connection accepted after local group change")
	}
}
