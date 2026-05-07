package logs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSizedFile(t *testing.T, root string, relativePath string, size int64) {
	t.Helper()
	path := filepath.Join(root, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", relativePath, err)
	}
	content := strings.Repeat("x", int(size))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", relativePath, err)
	}
}

func chtimes(path string, modified time.Time) error {
	return os.Chtimes(path, modified, modified)
}
