package mitm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/gklog"
	"gopkg.in/natefinch/lumberjack.v2"

	"goodkind.io/clyde/internal/config"
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

func TestAppendCaptureReusesRotatedWriter(t *testing.T) {
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

	clearCaptureWriterCacheForTest(t)
	if err := WriteCaptureLine(dir, []byte(`{"event":"one"}`), policy); err != nil {
		t.Fatalf("write capture line: %v", err)
	}
	if err := WriteCaptureLine(dir, []byte(`{"event":"two"}`), policy); err != nil {
		t.Fatalf("write second capture line: %v", err)
	}

	captureWriterCache.mu.Lock()
	writerCount := len(captureWriterCache.writers)
	captureWriterCache.mu.Unlock()
	if writerCount != 1 {
		t.Fatalf("capture writer count = %d, want 1", writerCount)
	}
}

func TestDefaultCapturePolicyDoesNotCompress(t *testing.T) {
	policy := captureFilePolicyFromConfig(config.MITMConfig{})
	if policy.Rotation.Compress == nil {
		t.Fatal("compress pointer is nil")
	}
	if *policy.Rotation.Compress {
		t.Fatal("default capture compression = true, want false")
	}
}

func clearCaptureWriterCacheForTest(t *testing.T) {
	t.Helper()
	captureWriterCache.mu.Lock()
	oldWriters := captureWriterCache.writers
	captureWriterCache.writers = make(map[captureWriterKey]*lumberjack.Logger)
	captureWriterCache.mu.Unlock()
	t.Cleanup(func() {
		captureWriterCache.mu.Lock()
		for _, writer := range captureWriterCache.writers {
			_ = writer.Close()
		}
		captureWriterCache.writers = oldWriters
		captureWriterCache.mu.Unlock()
	})
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
