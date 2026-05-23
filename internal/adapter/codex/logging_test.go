package codex

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCodexLogPathHonorsOverride(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "codex.jsonl")
	t.Setenv("CLYDE_CODEX_LOG_PATH", tmp)
	if got := CodexLogPath(); got != tmp {
		t.Fatalf("CodexLogPath=%q want %q", got, tmp)
	}
}

func TestCodexLogPathFromXDGState(t *testing.T) {
	t.Setenv("CLYDE_CODEX_LOG_PATH", "")
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-codex-test")
	want := "/tmp/xdg-codex-test/clyde/codex.jsonl"
	if got := CodexLogPath(); got != want {
		t.Fatalf("CodexLogPath=%q want %q", got, want)
	}
}

func TestLogCodexEventDoubleWritesToDedicatedSink(t *testing.T) {
	dir := t.TempDir()
	sinkPath := filepath.Join(dir, "codex.jsonl")
	t.Setenv("CLYDE_CODEX_LOG_PATH", sinkPath)
	resetDedicatedCodexLoggerForTest(t)

	attrs := []slog.Attr{
		slog.String("request_id", "req-test"),
		slog.String("cursor_request_id", "cursor-req-test"),
		slog.String("model", "gpt-5"),
		slog.Int("input_count", 1),
	}
	logCodexEvent(context.Background(), slog.LevelDebug, "adapter.codex.telemetry_probe", attrs)

	got, err := os.ReadFile(sinkPath)
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	if !strings.Contains(string(got), "adapter.codex.telemetry_probe") {
		t.Errorf("sink missing event name: %s", string(got))
	}
	if !strings.Contains(string(got), `"request_id":"req-test"`) {
		t.Errorf("sink missing request_id: %s", string(got))
	}
	if !strings.Contains(string(got), `"cursor_request_id":"cursor-req-test"`) {
		t.Errorf("sink missing cursor_request_id: %s", string(got))
	}
	if strings.Contains(string(got), `"body":`) || strings.Contains(string(got), "hello") {
		t.Errorf("sink leaked body bytes: %s", string(got))
	}
}

func TestDedicatedCodexLoggerRotatesByVolume(t *testing.T) {
	dir := t.TempDir()
	sinkPath := filepath.Join(dir, "codex.jsonl")
	t.Setenv("CLYDE_CODEX_LOG_PATH", sinkPath)
	resetDedicatedCodexLoggerForTest(t)

	compress := false
	ConfigureCodexFileLogger(FileLogRotationConfig{
		MaxSizeMB:  1,
		MaxBackups: 2,
		MaxAgeDays: 7,
		Compress:   &compress,
	})

	largeField := strings.Repeat("x", 700*1024)
	for range 3 {
		attrs := []slog.Attr{
			slog.String("request_id", "req-rotate"),
			slog.String("model", "gpt-5"),
			slog.String("probe", largeField),
		}
		logCodexEvent(context.Background(), slog.LevelDebug, "adapter.codex.rotation_probe", attrs)
	}
	if codexFileCloser != nil {
		if err := codexFileCloser.Close(); err != nil {
			t.Fatalf("close codex logger: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read log dir: %v", err)
	}
	var rotated int
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "codex-") && strings.HasSuffix(name, ".jsonl") {
			rotated++
		}
	}
	if rotated == 0 {
		t.Fatalf("expected rotated codex sidecar logs, entries=%v", entryNames(entries))
	}
}

func resetDedicatedCodexLoggerForTest(t *testing.T) {
	t.Helper()
	codexFileLoggerMu.Lock()
	defer codexFileLoggerMu.Unlock()
	if codexFileCloser != nil {
		_ = codexFileCloser.Close()
	}
	codexFileLoggerOnce = sync.Once{}
	codexFileLogger = nil
	codexFileCloser = nil
	codexFileRotation = FileLogRotationConfig{
		MaxSizeMB:  defaultCodexLogRotationMaxSizeMB,
		MaxBackups: defaultCodexLogRotationMaxBackups,
		MaxAgeDays: defaultCodexLogRotationMaxAgeDays,
		Compress:   new(true),
	}
	t.Cleanup(func() {
		codexFileLoggerMu.Lock()
		defer codexFileLoggerMu.Unlock()
		if codexFileCloser != nil {
			_ = codexFileCloser.Close()
		}
		codexFileLoggerOnce = sync.Once{}
		codexFileLogger = nil
		codexFileCloser = nil
		codexFileRotation = FileLogRotationConfig{
			MaxSizeMB:  defaultCodexLogRotationMaxSizeMB,
			MaxBackups: defaultCodexLogRotationMaxBackups,
			MaxAgeDays: defaultCodexLogRotationMaxAgeDays,
			Compress:   new(true),
		}
	})
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
