package replay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLowVideoPreservesAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cs2_video.txt")
	original := "\"video.cfg\"\n{\n\"VendorID\" \"4318\"\n"
	for key := range lowVideoSettings {
		original += fmt.Sprintf("\"%s\" \"9\"\n", key)
	}
	original += "}\n"
	os.WriteFile(path, []byte(original), 0640)
	if err := applyLowVideo(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"VendorID" "4318"`) {
		t.Fatal(string(b))
	}
	for key, value := range lowVideoSettings {
		if !strings.Contains(string(b), fmt.Sprintf("\"%s\" \"%s\"", key, value)) {
			t.Fatal(key, string(b))
		}
	}
	if err := applyLowVideo(path); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(path + ".backup-*")
	if len(backups) != 1 {
		t.Fatal(backups)
	}
	saved, _ := os.ReadFile(backups[0])
	if string(saved) != original {
		t.Fatal("backup mismatch")
	}
	os.WriteFile(path, []byte(`"video.cfg" {}`), 0600)
	if err := applyLowVideo(path); err == nil {
		t.Fatal("accepted missing fields")
	}
	b, _ = os.ReadFile(path)
	if string(b) != `"video.cfg" {}` {
		t.Fatal("modified invalid config")
	}
}
