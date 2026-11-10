package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLoggerWritesToStateDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	logger, cleanup := NewLogger("test-component", RotationConfig{
		MaxSizeMB:  4,
		MaxBackups: 1,
		MaxAgeDays: 3,
		Compress:   false,
	})
	defer cleanup()

	logger.Info("audit.test.event", "probe", "abc123")

	auditPath := filepath.Join(dir, "clyde", "audit.jsonl")
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "audit.test.event") {
		t.Fatalf("audit log missing event: %s", content)
	}
	if !strings.Contains(content, `"component":"test-component"`) {
		t.Fatalf("audit log missing component field: %s", content)
	}
	if !strings.Contains(content, `"probe":"abc123"`) {
		t.Fatalf("audit log missing probe field: %s", content)
	}
}

func TestRotationConfigNormalizedFillsZeroes(t *testing.T) {
	got := RotationConfig{}.normalized()
	if got.MaxSizeMB != defaultAuditRotationMaxSizeMB {
		t.Fatalf("max size MB = %d, want %d", got.MaxSizeMB, defaultAuditRotationMaxSizeMB)
	}
	if got.MaxBackups != defaultAuditRotationMaxBackups {
		t.Fatalf("max backups = %d, want %d", got.MaxBackups, defaultAuditRotationMaxBackups)
	}
	if got.MaxAgeDays != defaultAuditRotationMaxAgeDays {
		t.Fatalf("max age days = %d, want %d", got.MaxAgeDays, defaultAuditRotationMaxAgeDays)
	}
}

func TestRotationConfigNormalizedPreservesNonZero(t *testing.T) {
	in := RotationConfig{
		MaxSizeMB:  16,
		MaxBackups: 6,
		MaxAgeDays: 3,
		Compress:   true,
	}
	got := in.normalized()
	if got != in {
		t.Fatalf("normalized changed values: got=%+v want=%+v", got, in)
	}
}
