package mitm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/gklog"
)

func TestAppendCaptureRotatesCaptureIndex(t *testing.T) {
	dir := t.TempDir()
	compress := false
	policy := CaptureFilePolicy{
		RotationEnabled: true,
		Rotation: gklog.RotationConfig{
			MaxSizeMB:  1,
			MaxBackups: 2,
			MaxAgeDays: 1,
			Compress:   &compress,
		},
	}
	line := bytes.Repeat([]byte("x"), 600*1024)
	for i := 0; i < 3; i++ {
		if err := WriteCaptureLine(dir, line, policy); err != nil {
			t.Fatalf("append capture line %d: %v", i, err)
		}
	}

	activePath := filepath.Join(dir, captureIndexFilename)
	activeInfo, err := os.Stat(activePath)
	if err != nil {
		t.Fatalf("stat active capture index: %v", err)
	}
	if activeInfo.Size() >= 1024*1024 {
		t.Fatalf("active capture index size=%d, want rotated file below 1 MiB", activeInfo.Size())
	}

	rotated := findFilesWithPrefix(t, dir, "capture-")
	if len(rotated) == 0 {
		t.Fatalf("expected at least one rotated capture index in %s", dir)
	}
}

func findFilesWithPrefix(t *testing.T, dir string, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if len(entry.Name()) >= len(prefix) && entry.Name()[:len(prefix)] == prefix {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	return paths
}
