// Package codex contains Codex transport and runtime integration.
// The package keeps a dedicated logging surface so payload corruption on the
// Codex path is observable end-to-end. The dedicated JSONL sidecar lives at
// $XDG_STATE_HOME/clyde/codex.jsonl and uses locked volume-based rotation.
package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog"
)

// FileLogRotationConfig controls the dedicated Codex JSONL sidecar sink.
// It mirrors internal/config.LoggingRotation without importing config into
// the provider package.
type FileLogRotationConfig struct {
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   *bool
}

const (
	defaultCodexLogRotationMaxSizeMB  = 64
	defaultCodexLogRotationMaxBackups = 192
	defaultCodexLogRotationMaxAgeDays = 14
)

// CodexLogPath returns the JSONL file the codex package double-writes
// its wire events to. Honors $CLYDE_CODEX_LOG_PATH for tests.
func CodexLogPath() string {
	if p := os.Getenv("CLYDE_CODEX_LOG_PATH"); p != "" {
		return p
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "clyde", "codex.jsonl")
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "clyde", "codex.jsonl")
}

var (
	codexFileLoggerMu   sync.Mutex
	codexFileLoggerOnce sync.Once
	codexFileLogger     *slog.Logger
	codexFileCloser     io.Closer
	codexFileRotation   = FileLogRotationConfig{
		MaxSizeMB:  defaultCodexLogRotationMaxSizeMB,
		MaxBackups: defaultCodexLogRotationMaxBackups,
		MaxAgeDays: defaultCodexLogRotationMaxAgeDays,
		Compress:   new(true),
	}
)

// ConfigureCodexFileLogger installs rotation settings for codex.jsonl before
// the first Codex event is emitted. Later calls are ignored because slog
// handlers bind their writer path and lumberjack settings at construction.
func ConfigureCodexFileLogger(rotation FileLogRotationConfig) {
	codexFileLoggerMu.Lock()
	defer codexFileLoggerMu.Unlock()
	if codexFileLogger != nil {
		return
	}
	codexFileRotation = normalizeCodexLogRotation(rotation)
}

// dedicatedCodexLogger returns a JSON slog handler writing to
// CodexLogPath(). Best effort: a missing log dir never blocks traffic. The
// handler is bound to the path and rotation config observed at first call.
func dedicatedCodexLogger() *slog.Logger {
	codexFileLoggerMu.Lock()
	defer codexFileLoggerMu.Unlock()
	codexFileLoggerOnce.Do(func() {
		path := CodexLogPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return
		}
		handler := gklog.FileJSON(path, slog.LevelDebug, codexFileRotation.toGKLog())
		if closer, ok := handler.(io.Closer); ok {
			codexFileCloser = closer
		}
		codexFileLogger = slog.New(handler)
	})
	return codexFileLogger
}

func normalizeCodexLogRotation(rotation FileLogRotationConfig) FileLogRotationConfig {
	if rotation.MaxSizeMB <= 0 {
		rotation.MaxSizeMB = defaultCodexLogRotationMaxSizeMB
	}
	if rotation.MaxBackups <= 0 {
		rotation.MaxBackups = defaultCodexLogRotationMaxBackups
	}
	if rotation.MaxAgeDays <= 0 {
		rotation.MaxAgeDays = defaultCodexLogRotationMaxAgeDays
	}
	if rotation.Compress == nil {
		rotation.Compress = new(true)
	}
	return rotation
}

func (c FileLogRotationConfig) toGKLog() gklog.RotationConfig {
	c = normalizeCodexLogRotation(c)
	return gklog.RotationConfig{
		MaxSizeMB:  c.MaxSizeMB,
		MaxBackups: c.MaxBackups,
		MaxAgeDays: c.MaxAgeDays,
		Compress:   c.Compress,
	}
}

// logCodexEvent writes the event to both slog.Default() (which the
// daemon configures to fan out to clyde-daemon.jsonl) and the
// dedicated codex.jsonl sink. The dedicated file is best effort.
func logCodexEvent(ctx context.Context, level slog.Level, event string, attrs []slog.Attr) {
	logCodexEventWithConcern(ctx, level, event, slogger.ConcernAdapterProviderCodex, attrs)
}

func logCodexEventWithConcern(ctx context.Context, level slog.Level, event, concern string, attrs []slog.Attr) {
	if ctx == nil {
		ctx = context.Background()
	}
	slogger.WithConcern(slog.Default(), concern).LogAttrs(ctx, level, event, attrs...)
	if l := dedicatedCodexLogger(); l != nil {
		slogger.WithConcern(l, concern).LogAttrs(ctx, level, event, attrs...)
	}
}

func intMapAttrs(values map[string]int) []slog.Attr {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	attrs := make([]slog.Attr, 0, len(keys))
	for _, key := range keys {
		attrs = append(attrs, slog.Int(key, values[key]))
	}
	return attrs
}

func sha256StringHex(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return sha256Hex([]byte(value))
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
