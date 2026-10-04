package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testService(t *testing.T, role string) *Service {
	t.Helper()
	s, e := New(t.TempDir(), role)
	if e != nil {
		t.Fatal(e)
	}
	s.s.Config.Map = "de_mirage" // Synthetic test fixture, not a production default.
	s.s.Config.Mode = "demo"     // Existing synthetic pipeline fixtures never control CS2.
	t.Cleanup(s.Close)
	return s
}
func request(t *testing.T, s *Service, method, path string, body any, token, origin string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestDirectAccessAndOriginProtection(t *testing.T) {
	s := testService(t, "director")
	if w := request(t, s, "GET", "/api/state", nil, "", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request(t, s, "POST", "/api/demo", nil, "", "https://evil.example"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	w := request(t, s, "GET", "/api/state", nil, "", "")
	if w.Code != 200 {
		t.Fatal("state request failed")
	}
	if w := request(t, s, "GET", "/api/media/../../access-token", nil, "", ""); w.Code == 200 {
		t.Fatal("path traversal")
	}
}
func TestUpsertAndRecovery(t *testing.T) {
	s := testService(t, "director")
	e := ev("one", "A", nowMS()+10000)
	if err := s.upsert(e); err != nil {
		t.Fatal(err)
	}
	e.Name = "revised"
	if err := s.upsert(e); err != nil {
		t.Fatal(err)
	}
	if len(s.s.Events) != 1 || s.s.Events[0].Name != "revised" {
		t.Fatal("not idempotent")
	}
	s.s.Events[0].JobID = "job"
	s.s.Jobs = append(s.s.Jobs, Job{ID: "job", Status: "CAPTURING"})
	s.saveLocked()
	s.Close()
	s2, err := New(s.dir, "director")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.s.Config.Paused || s2.s.Jobs[0].Status != "FAILED" || s2.s.Events[0].Status != "FAILED" {
		t.Fatal("unsafe recovery")
	}
}
func TestDemoPipelineAndRange(t *testing.T) {
	s := testService(t, "director")
	s.s.Config.Setup = .2
	s.s.Config.Guard = .1
	s.s.Config.Paused = false
	e := ev("first", "A", nowMS()+1600)
	e.Uncertainty = 0
	if err := s.upsert(e); err != nil {
		t.Fatal(err)
	}
	s.Start()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		ready := len(s.s.Artifacts) > 0
		s.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	s.mu.Lock()
	if len(s.s.Artifacts) != 1 {
		t.Fatalf("pipeline failed: %+v", s.s)
	}
	art := s.s.Artifacts[0]
	s.mu.Unlock()
	hash, size, err := fileHash(art.Path)
	if err != nil || hash != art.SHA256 || size != art.Size {
		t.Fatal("hash mismatch")
	}
	if !art.Demo || art.Quality != "demo" {
		t.Fatal("demo not labelled")
	}
	r := httptest.NewRequest("GET", "/api/media/"+art.ID, nil)
	r.Header.Set("Range", "bytes=0-9")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 206 || w.Body.Len() != 10 {
		t.Fatal(w.Code, w.Body.Len())
	}
	// The release embeds actual H.264/AAC; check it with the installed media probe.
	if os.Getenv("REPLAY_MEDIA_TEST") == "1" {
		duration, e := probe(context.Background(), "ffprobe", art.Path)
		if e != nil || duration < 1.45 || duration > 1.58 {
			t.Fatal(duration, e)
		}
	}
}
func TestGSIBatchDoesNotInventKills(t *testing.T) {
	s := testService(t, "director")
	p := func(k int) map[string]any {
		b := []byte(`{"provider":{"appid":730},"map":{"name":"de_mirage","round":1},"allplayers":{"A":{"name":"Player","state":{"round_kills":0}}}}`)
		var m map[string]any
		json.Unmarshal(b, &m)
		obj(obj(obj(m, "allplayers"), "A"), "state")["round_kills"] = float64(k)
		return m
	}
	if e := s.ingestGSI("a", p(0)); e != nil {
		t.Fatal(e)
	}
	if e := s.ingestGSI("a", p(2)); e != nil {
		t.Fatal(e)
	}
	if len(s.s.Events) != 1 || s.s.Events[0].GroupCount != 2 || s.s.Events[0].Tick != nil {
		t.Fatal(s.s.Events)
	}
	s.ingestGSI("a", p(2))
	if len(s.s.Events) != 1 {
		t.Fatal("duplicate snapshot created events")
	}
}
func TestDownloadResumeAndRejectCorruption(t *testing.T) {
	s := testService(t, "director")
	source := filepath.Join(t.TempDir(), "x.mp4")
	os.WriteFile(source, demoVideo, 0600)
	hash, size, _ := fileHash(source)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(401)
			return
		}
		http.ServeContent(w, r, "video.mp4", time.Time{}, bytes.NewReader(demoVideo))
	}))
	defer server.Close()
	c := defaults()
	c.WorkerURL = server.URL
	art := Artifact{ID: "download", Size: size, SHA256: hash}
	os.WriteFile(filepath.Join(s.dir, "media", "download.mp4.part"), demoVideo[:1000], 0600)
	if e := s.download(c, &art); e != nil {
		t.Fatal(e)
	}
	if hash2, _, _ := fileHash(art.Path); hash2 != hash {
		t.Fatal("resume corrupted")
	}
	art.ID = "bad"
	art.SHA256 = strings.Repeat("0", 64)
	if e := s.download(c, &art); e == nil {
		t.Fatal("accepted corrupt artifact")
	}
	if _, e := os.Stat(filepath.Join(s.dir, "media", "bad.mp4")); !os.IsNotExist(e) {
		t.Fatal("published corrupt artifact")
	}
}
func TestAgentValidationAndDuplicate(t *testing.T) {
	s := testService(t, "agent")
	s.s.Config.Paused = false
	s.s.Config.CalibratedUntil = nowMS() + 60000
	j := Job{ID: "same", Match: s.s.Config.Match, Map: s.s.Config.Map, Epoch: 1, Player: "A", Demo: true, Start: nowMS() + 10000, End: nowMS() + 13000, Events: []Event{ev("e", "A", nowMS()+11500)}}
	got, e := s.acceptJob(j)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.acceptJob(j)
	if e != nil || got.ID != again.ID || len(s.s.Jobs) != 1 {
		t.Fatal("duplicate accepted twice", e)
	}
	j.ID = "other"
	if _, e = s.acceptJob(j); e == nil {
		t.Fatal("accepted conflicting task")
	}
}
func TestNoLoginOrAccessTokenFile(t *testing.T) {
	s := testService(t, "director")
	if _, err := os.Stat(filepath.Join(s.dir, "access-token")); !os.IsNotExist(err) {
		t.Fatal("access-token file should not exist", err)
	}
	if w := request(t, s, "POST", "/login", nil, "", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := request(t, s, "GET", "/api/state", nil, "", ""); w.Code != 200 || len(w.Result().Cookies()) != 0 {
		t.Fatal(w.Code)
	}
}

func TestDataDirectoryExclusive(t *testing.T) {
	s := testService(t, "director")
	if other, e := New(s.dir, "director"); e == nil {
		other.Close()
		t.Fatal("same directory opened twice")
	}
}
func TestTwoNodeDemoTransfer(t *testing.T) {
	worker := testService(t, "agent")
	worker.s.Config.Setup = .2
	worker.s.Config.Paused = false
	worker.s.Config.CalibratedUntil = nowMS() + 60000
	server := httptest.NewServer(worker.Handler())
	defer server.Close()
	director := testService(t, "director")
	director.s.Config.WorkerURL = server.URL
	if e := director.syncRemote(); e != nil {
		t.Fatal(e)
	}
	event := ev("remoteevent", "A", nowMS()+1700)
	j := Job{ID: "remotejob", Epoch: 1, Match: event.Match, Map: event.Map, Player: "A", Demo: true, Start: event.Time - 1200, End: event.Time + 1200, Events: []Event{event}, Status: "COMMITTED"}
	director.s.Jobs = []Job{j}
	director.runRemote(j, director.s.Config)
	if len(director.s.Artifacts) != 1 || director.s.Jobs[0].Status != "READY" {
		t.Fatalf("remote pipeline failed: %+v", director.s.Jobs)
	}
	got := director.s.Artifacts[0]
	if !strings.HasPrefix(got.Path, director.dir) {
		t.Fatal("did not download to director")
	}
	hash, _, e := fileHash(got.Path)
	if e != nil || hash != got.SHA256 {
		t.Fatal("download hash failed")
	}
}
func TestEmptyPlannerSerializesArray(t *testing.T) {
	b, _ := json.Marshal(Plan([]Event{}, nil, testConfig(), 0))
	if string(b) != "[]" {
		t.Fatal(string(b))
	}
}
func TestConflictReasonSurvivesDeadline(t *testing.T) {
	a, b, c := ev("a", "A", 10000), ev("b", "B", 10200), ev("c", "B", 10700)
	first := Plan([]Event{a, b, c}, nil, testConfig(), 0)
	later := Plan(first, nil, testConfig(), 20000)
	if later[0].Status != "CONFLICT" {
		t.Fatal(later[0])
	}
}

func TestLegacyPairingDataRemoved(t *testing.T) {
	a := testService(t, "director")
	a.Close()
	path := filepath.Join(a.dir, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	cfg := old["state"].(map[string]any)["config"].(map[string]any)
	for _, key := range []string{"worker_token", "gsi_token", "tracking_token"} {
		cfg[key] = "obsolete-secret"
	}
	data, _ = json.Marshal(old)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.dir, "access-token"), []byte("obsolete-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := New(a.dir, "director")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := os.Stat(filepath.Join(a.dir, "access-token")); !os.IsNotExist(err) {
		t.Fatal("legacy credential retained", err)
	}
	data, err = os.ReadFile(path)
	if err != nil || bytes.Contains(data, []byte("obsolete-secret")) {
		t.Fatal("legacy config not migrated", err)
	}
}
