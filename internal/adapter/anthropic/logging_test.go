package anthropic

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func TestLogPathHonorsOverride(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "anthropic.jsonl")
	t.Setenv("CLYDE_ANTHROPIC_LOG_PATH", tmp)
	if got := LogPath(); got != tmp {
		t.Fatalf("LogPath=%q want %q", got, tmp)
	}
}

func TestLogPathFromXDGState(t *testing.T) {
	t.Setenv("CLYDE_ANTHROPIC_LOG_PATH", "")
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-anthropic-test")
	want := "/tmp/xdg-anthropic-test/clyde/anthropic.jsonl"
	if got := LogPath(); got != want {
		t.Fatalf("LogPath=%q want %q", got, want)
	}
}

func TestLogPathExpandsXDGStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLYDE_ANTHROPIC_LOG_PATH", "")
	t.Setenv("XDG_STATE_HOME", "~/state/../state-root")
	want := filepath.Join(home, "state-root", "clyde", "anthropic.jsonl")
	if got := LogPath(); got != want {
		t.Fatalf("LogPath=%q want %q", got, want)
	}
}

func TestLogResponseDoubleWritesToDedicatedSink(t *testing.T) {
	dir := t.TempDir()
	sinkPath := filepath.Join(dir, "anthropic.jsonl")
	t.Setenv("CLYDE_ANTHROPIC_LOG_PATH", sinkPath)
	resetDedicatedAnthropicLoggerForTest(t)

	logResponse(context.Background(), slog.LevelInfo, "anthropic.messages.response", responseEvent{
		Subcomponent: "anthropic",
		Model:        "claude-sonnet-4-5",
		Status:       200,
		RequestID:    "req-test",
		RequestBytes: 42,
	})

	got, err := os.ReadFile(sinkPath)
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	if !strings.Contains(string(got), "anthropic.messages.response") {
		t.Errorf("sink missing event name: %s", string(got))
	}
	if !strings.Contains(string(got), `"request_id":"req-test"`) {
		t.Errorf("sink missing request_id: %s", string(got))
	}
}

func TestDedicatedAnthropicLoggerRotatesByVolume(t *testing.T) {
	dir := t.TempDir()
	sinkPath := filepath.Join(dir, "anthropic.jsonl")
	t.Setenv("CLYDE_ANTHROPIC_LOG_PATH", sinkPath)
	resetDedicatedAnthropicLoggerForTest(t)

	compress := false
	ConfigureAnthropicFileLogger(FileLogRotationConfig{
		MaxSizeMB:  1,
		MaxBackups: 2,
		MaxAgeDays: 3,
		Compress:   &compress,
	})

	// Response bodies are never logged, so volume is driven by a large logged
	// error string instead: three oversized error events push the sidecar past
	// its 1 MB cap and force at least one rotation.
	largeErr := strings.Repeat("x", 700*1024)
	for range 3 {
		logResponse(context.Background(), slog.LevelError, "anthropic.messages.upstream_error", responseEvent{
			Subcomponent:  "anthropic",
			Model:         "claude-sonnet-4-5",
			Status:        500,
			RequestID:     "req-rotate",
			ResponseBytes: len(largeErr),
			Err:           largeErr,
		})
	}
	if fileLoggerCloser != nil {
		if err := fileLoggerCloser.Close(); err != nil {
			t.Fatalf("close anthropic logger: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read log dir: %v", err)
	}
	var rotated int
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "anthropic-") && strings.HasSuffix(name, ".jsonl") {
			rotated++
		}
	}
	if rotated == 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("expected rotated anthropic sidecar logs, entries=%v", names)
	}
}

func TestConfigureAnthropicFileLoggerNormalizesDefaults(t *testing.T) {
	resetDedicatedAnthropicLoggerForTest(t)
	ConfigureAnthropicFileLogger(FileLogRotationConfig{})
	if fileLoggerRotation.MaxSizeMB != config.SidecarRotationMaxSizeMB {
		t.Fatalf("max size MB = %d, want %d", fileLoggerRotation.MaxSizeMB, config.SidecarRotationMaxSizeMB)
	}
	if fileLoggerRotation.MaxBackups != config.SidecarRotationMaxBackups {
		t.Fatalf("max backups = %d, want %d", fileLoggerRotation.MaxBackups, config.SidecarRotationMaxBackups)
	}
	if fileLoggerRotation.MaxAgeDays != config.SidecarRotationMaxAgeDays {
		t.Fatalf("max age days = %d, want %d", fileLoggerRotation.MaxAgeDays, config.SidecarRotationMaxAgeDays)
	}
	if fileLoggerRotation.Compress == nil || !*fileLoggerRotation.Compress {
		t.Fatalf("compress should default to true")
	}
}

func resetDedicatedAnthropicLoggerForTest(t *testing.T) {
	t.Helper()
	fileLoggerMu.Lock()
	defer fileLoggerMu.Unlock()
	if fileLoggerCloser != nil {
		_ = fileLoggerCloser.Close()
	}
	fileLoggerOnce = sync.Once{}
	fileLogger = nil
	fileLoggerCloser = nil
	fileLoggerRotation = FileLogRotationConfig{
		MaxSizeMB:  config.SidecarRotationMaxSizeMB,
		MaxBackups: config.SidecarRotationMaxBackups,
		MaxAgeDays: config.SidecarRotationMaxAgeDays,
		Compress:   newBool(true),
	}
	t.Cleanup(func() {
		fileLoggerMu.Lock()
		defer fileLoggerMu.Unlock()
		if fileLoggerCloser != nil {
			_ = fileLoggerCloser.Close()
		}
		fileLoggerOnce = sync.Once{}
		fileLogger = nil
		fileLoggerCloser = nil
		fileLoggerRotation = FileLogRotationConfig{
			MaxSizeMB:  config.SidecarRotationMaxSizeMB,
			MaxBackups: config.SidecarRotationMaxBackups,
			MaxAgeDays: config.SidecarRotationMaxAgeDays,
			Compress:   newBool(true),
		}
	})
}

func newBool(value bool) *bool {
	return &value
}
