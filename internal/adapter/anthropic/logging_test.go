package anthropic

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAnthropicLogPathHonorsOverride(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "anthropic.jsonl")
	t.Setenv("CLYDE_ANTHROPIC_LOG_PATH", tmp)
	if got := AnthropicLogPath(); got != tmp {
		t.Fatalf("AnthropicLogPath=%q want %q", got, tmp)
	}
}

func TestAnthropicLogPathFromXDGState(t *testing.T) {
	t.Setenv("CLYDE_ANTHROPIC_LOG_PATH", "")
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-anthropic-test")
	want := "/tmp/xdg-anthropic-test/clyde/anthropic.jsonl"
	if got := AnthropicLogPath(); got != want {
		t.Fatalf("AnthropicLogPath=%q want %q", got, want)
	}
}

func TestLogResponseDoubleWritesToDedicatedSink(t *testing.T) {
	dir := t.TempDir()
	sinkPath := filepath.Join(dir, "anthropic.jsonl")
	t.Setenv("CLYDE_ANTHROPIC_LOG_PATH", sinkPath)
	resetDedicatedAnthropicLoggerForTest(t)

	logResponse(slog.LevelInfo, "anthropic.messages.response", responseEvent{
		Subcomponent: "anthropic",
		Model:        "claude-sonnet-4-5",
		Status:       200,
		RequestID:    "req-test",
		BodyBytes:    42,
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

	largeBody := strings.Repeat("x", 700*1024)
	for range 3 {
		logResponse(slog.LevelInfo, "anthropic.messages.response", responseEvent{
			Subcomponent: "anthropic",
			Model:        "claude-sonnet-4-5",
			Status:       200,
			RequestID:    "req-rotate",
			BodyBytes:    len(largeBody),
			Body:         largeBody,
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
	if fileLoggerRotation.MaxSizeMB != defaultAnthropicLogRotationMaxSizeMB {
		t.Fatalf("max size MB = %d, want %d", fileLoggerRotation.MaxSizeMB, defaultAnthropicLogRotationMaxSizeMB)
	}
	if fileLoggerRotation.MaxBackups != defaultAnthropicLogRotationMaxBackups {
		t.Fatalf("max backups = %d, want %d", fileLoggerRotation.MaxBackups, defaultAnthropicLogRotationMaxBackups)
	}
	if fileLoggerRotation.MaxAgeDays != defaultAnthropicLogRotationMaxAgeDays {
		t.Fatalf("max age days = %d, want %d", fileLoggerRotation.MaxAgeDays, defaultAnthropicLogRotationMaxAgeDays)
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
		MaxSizeMB:  defaultAnthropicLogRotationMaxSizeMB,
		MaxBackups: defaultAnthropicLogRotationMaxBackups,
		MaxAgeDays: defaultAnthropicLogRotationMaxAgeDays,
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
			MaxSizeMB:  defaultAnthropicLogRotationMaxSizeMB,
			MaxBackups: defaultAnthropicLogRotationMaxBackups,
			MaxAgeDays: defaultAnthropicLogRotationMaxAgeDays,
			Compress:   newBool(true),
		}
	})
}

func newBool(value bool) *bool {
	return &value
}
