package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArtifactIDValidation(t *testing.T) {
	s, err := NewServer(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tc := range []struct {
		name, id string
		valid    bool
	}{
		{"legacy", "clip_1-A", true},
		{"recorded", strings.Repeat("a", 32) + "-" + strings.Repeat("b", 32), true},
		{"maximum", strings.Repeat("a", 100), true},
		{"too-long", strings.Repeat("a", 101), false},
		{"empty", "", false},
		{"traversal", "../clip", false},
		{"slash", "clip/other", false},
		{"dot", "clip.mp4", false},
		{"unicode", "素材", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/media/test", nil)
			r.SetPathValue("id", tc.id)
			r.Header.Set("X-Replay-SHA256", strings.Repeat("a", 64))
			r.Header.Set("X-Replay-Size", "123")
			w := httptest.NewRecorder()
			s.media(w, r)
			want := http.StatusBadRequest
			if tc.valid {
				// Accepted IDs reach the pairing check; no peer is connected.
				want = http.StatusConflict
			}
			if w.Code != want {
				t.Fatalf("status = %d, want %d: %s", w.Code, want, w.Body.String())
			}
		})
	}
	if IDPattern.MatchString(strings.Repeat("a", 65)) {
		t.Fatal("device ID limit must remain 64")
	}
}
