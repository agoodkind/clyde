package slogger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSelectCleanupCandidatesHonorsAgeBackupsAndTotalSize(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	files := []cleanupFile{
		{path: "/tmp/a/clyde-daemon.jsonl", group: "/tmp/a/clyde-daemon.jsonl", size: 4, modTime: now},
		{path: "/tmp/a/clyde-daemon-1.jsonl", group: "/tmp/a/clyde-daemon.jsonl", size: 4, modTime: now.Add(-time.Hour)},
		{path: "/tmp/a/clyde-daemon-2.jsonl", group: "/tmp/a/clyde-daemon.jsonl", size: 4, modTime: now.Add(-2 * time.Hour)},
		{path: "/tmp/a/old.jsonl", group: "/tmp/a/old.jsonl", size: 4, modTime: now.Add(-10 * 24 * time.Hour)},
	}

	candidates := selectCleanupCandidates(files, CleanupPolicy{Enabled: true, MaxAgeDays: 7, MaxBackups: 2, MaxTotalMB: 1}, now)

	if len(candidates) != 2 {
		t.Fatalf("candidates=%v want 2", candidates)
	}
	if candidates[0].path != "/tmp/a/clyde-daemon-2.jsonl" || candidates[1].path != "/tmp/a/old.jsonl" {
		t.Fatalf("candidate paths=%v", candidates)
	}
}

func TestApplyCleanupPolicyTolerantOfMissingRoot(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "does-not-exist")
	policy := CleanupPolicy{Enabled: true, Root: missingRoot, MaxAgeDays: 7}
	if err := applyCleanupPolicy(policy); err != nil {
		t.Fatalf("applyCleanupPolicy on missing root returned err=%v, want nil", err)
	}
	if _, err := os.Stat(missingRoot); !os.IsNotExist(err) {
		t.Fatalf("missing root unexpectedly exists after apply: %v", err)
	}
}

func TestApplyCleanupPolicyDeletesOldLogs(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "logs", "adapter", "old.jsonl")
	newPath := filepath.Join(root, "logs", "adapter", "new.jsonl")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o600); err != nil {
		t.Fatalf("write new: %v", err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	policy := CleanupPolicy{Enabled: true, Root: root, MaxAgeDays: 1}
	if err := applyCleanupPolicy(policy); err != nil {
		t.Fatalf("applyCleanupPolicy: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old path stat err=%v want not exist", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new path stat: %v", err)
	}
}

func TestApplyCleanupPolicyDeletesOldRawCaptures(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "mitm", "raw", "api.anthropic.com", "old.raw")
	newPath := filepath.Join(root, "mitm", "raw", "api.anthropic.com", "new.raw")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("write old raw: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("new"), 0o600); err != nil {
		t.Fatalf("write new raw: %v", err)
	}
	oldTime := cleanupPolicyNow().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	policy := CleanupPolicy{Enabled: true, Root: root, MaxAgeDays: 1}
	if err := applyCleanupPolicy(policy); err != nil {
		t.Fatalf("applyCleanupPolicy: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old raw stat err=%v want not exist", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new raw stat: %v", err)
	}
}

func TestApplyCleanupPolicyEmitsCompletedEventWithTypedFields(t *testing.T) {
	root := t.TempDir()
	processLog := filepath.Join(root, "clyde-daemon.jsonl")
	concernLog := filepath.Join(root, "logs", "adapter", "adapter.http.jsonl")
	requestLog := filepath.Join(root, "logs", "requests", "req-1.jsonl")
	chatLog := filepath.Join(root, "logs", "chats", "chat-1.jsonl")
	sidecar := filepath.Join(root, "codex.jsonl")
	captureIndex := filepath.Join(root, "mitm", "always-on", "capture.jsonl")
	rawCapture := filepath.Join(root, "mitm", "raw", "api.anthropic.com", "trace.raw")
	inventoryIndex := filepath.Join(root, "logs", "inventory", "events.jsonl")
	fresh := filepath.Join(root, "logs", "adapter", "fresh.jsonl")

	for _, path := range []string{processLog, concernLog, requestLog, chatLog, sidecar, captureIndex, rawCapture, inventoryIndex, fresh} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("content-of-"+filepath.Base(path)), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	oldTime := cleanupPolicyNow().Add(-48 * time.Hour)
	for _, path := range []string{processLog, concernLog, requestLog, chatLog, sidecar, captureIndex, rawCapture, inventoryIndex} {
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}

	var buffer bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(original) })

	policy := CleanupPolicy{Enabled: true, Root: root, MaxAgeDays: 1}
	if err := applyCleanupPolicy(policy); err != nil {
		t.Fatalf("applyCleanupPolicy: %v", err)
	}

	completed := parseLogEvent(t, buffer.String(), "slogger.cleanup.completed")
	if completed == nil {
		t.Fatalf("did not find completed event in: %s", buffer.String())
	}
	roots, ok := completed["scanned_roots"].([]any)
	if !ok || len(roots) != 1 || roots[0] != root {
		t.Fatalf("scanned_roots=%v want [%s]", completed["scanned_roots"], root)
	}
	if candidates, ok := completed["candidates"].(float64); !ok || candidates != 8 {
		t.Fatalf("candidates=%v want 8", completed["candidates"])
	}
	if deleted, ok := completed["deleted"].(float64); !ok || deleted != 8 {
		t.Fatalf("deleted=%v want 8", completed["deleted"])
	}
	bytesDeleted, ok := completed["bytes_deleted"].(float64)
	if !ok || bytesDeleted <= 0 {
		t.Fatalf("bytes_deleted=%v want > 0", completed["bytes_deleted"])
	}
	if skipped, ok := completed["skipped"].([]any); !ok || len(skipped) != 0 {
		t.Fatalf("skipped=%v want empty slice", completed["skipped"])
	}
	if errs, ok := completed["errors"].([]any); !ok || len(errs) != 0 {
		t.Fatalf("errors=%v want empty slice", completed["errors"])
	}
	if duration, ok := completed["duration_ms"].(float64); !ok || duration <= 0 {
		t.Fatalf("duration_ms=%v want > 0", completed["duration_ms"])
	}

	started := parseLogEvent(t, buffer.String(), "slogger.cleanup.started")
	if started == nil {
		t.Fatalf("did not find started event in: %s", buffer.String())
	}
	if startRoots, ok := started["scanned_roots"].([]any); !ok || len(startRoots) != 1 || startRoots[0] != root {
		t.Fatalf("started.scanned_roots=%v want [%s]", started["scanned_roots"], root)
	}

	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh path stat: %v want nil (still present)", err)
	}
}

func TestApplyCleanupPolicyReportsSkippedAndErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based permission tests not supported on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses chmod permission checks")
	}
	root := t.TempDir()
	lockedDir := filepath.Join(root, "logs", "locked")
	if err := os.MkdirAll(lockedDir, 0o755); err != nil {
		t.Fatalf("mkdir locked: %v", err)
	}
	lockedFile := filepath.Join(lockedDir, "permission-denied.jsonl")
	if err := os.WriteFile(lockedFile, []byte("locked"), 0o600); err != nil {
		t.Fatalf("write locked: %v", err)
	}
	oldTime := cleanupPolicyNow().Add(-48 * time.Hour)
	if err := os.Chtimes(lockedFile, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes locked: %v", err)
	}
	if err := os.Chmod(lockedDir, 0o555); err != nil {
		t.Fatalf("chmod locked dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o755) })

	var buffer bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(original) })

	policy := CleanupPolicy{Enabled: true, Root: root, MaxAgeDays: 1}
	if err := applyCleanupPolicy(policy); err == nil {
		t.Fatalf("applyCleanupPolicy returned nil; want error from skipped file")
	}

	completed := parseLogEvent(t, buffer.String(), "slogger.cleanup.completed")
	if completed == nil {
		t.Fatalf("did not find completed event in: %s", buffer.String())
	}
	if deleted, ok := completed["deleted"].(float64); !ok || deleted != 0 {
		t.Fatalf("deleted=%v want 0", completed["deleted"])
	}
	if candidates, ok := completed["candidates"].(float64); !ok || candidates != 1 {
		t.Fatalf("candidates=%v want 1", completed["candidates"])
	}
	skipped, ok := completed["skipped"].([]any)
	if !ok || len(skipped) != 1 {
		t.Fatalf("skipped=%v want one entry", completed["skipped"])
	}
	if skippedStr, ok := skipped[0].(string); !ok || skippedStr != lockedFile {
		t.Fatalf("skipped[0]=%v want %s", skipped[0], lockedFile)
	}
	errs, ok := completed["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("errors=%v want at least one entry", completed["errors"])
	}
}

func parseLogEvent(t *testing.T, output string, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		event := make(map[string]any)
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event["msg"] == msg {
			return event
		}
	}
	return nil
}
