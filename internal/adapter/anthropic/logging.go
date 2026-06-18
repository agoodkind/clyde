package anthropic

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/gklog"
)

// FileLogRotationConfig controls the dedicated Anthropic JSONL sidecar sink.
// It mirrors internal/config.LoggingRotation without importing config into
// the provider package, matching the Codex sidecar pattern.
type FileLogRotationConfig struct {
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   *bool
}

// normalizeAnthropicLogRotation clamps the sidecar rotation to the shared
// provider-sidecar defaults via [config.NormalizeSidecarRotation] so the
// anthropic and codex sidecars cannot drift apart on retention policy.
func normalizeAnthropicLogRotation(rotation FileLogRotationConfig) FileLogRotationConfig {
	normalized := config.NormalizeSidecarRotation(config.SidecarRotation{
		MaxSizeMB:  rotation.MaxSizeMB,
		MaxBackups: rotation.MaxBackups,
		MaxAgeDays: rotation.MaxAgeDays,
		Compress:   rotation.Compress,
	})
	return FileLogRotationConfig{
		MaxSizeMB:  normalized.MaxSizeMB,
		MaxBackups: normalized.MaxBackups,
		MaxAgeDays: normalized.MaxAgeDays,
		Compress:   normalized.Compress,
	}
}

func (c FileLogRotationConfig) toGKLog() gklog.RotationConfig {
	c = normalizeAnthropicLogRotation(c)
	return gklog.RotationConfig{
		MaxSizeMB:  c.MaxSizeMB,
		MaxBackups: c.MaxBackups,
		MaxAgeDays: c.MaxAgeDays,
		Compress:   c.Compress,
	}
}

// rateLimitAttr is one anthropic-ratelimit-* response header captured
// alongside a /v1/messages response. Kept as a typed pair so the
// response event struct stays free of []any until the very last
// moment when slog needs the variadic shape.
type rateLimitAttr struct {
	Name  string
	Value string
}

// rateLimitAttrs extracts vendor rate-limit headers as typed pairs for logging.
func rateLimitAttrs(h http.Header) []rateLimitAttr {
	attrs := make([]rateLimitAttr, 0, 8)
	for key, values := range h {
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, "anthropic-ratelimit-") {
			continue
		}
		if len(values) == 0 {
			continue
		}
		attrs = append(attrs, rateLimitAttr{Name: lower, Value: values[0]})
	}
	return attrs
}

var (
	fileLoggerMu       sync.Mutex
	fileLoggerOnce     sync.Once
	fileLogger         *slog.Logger
	fileLoggerCloser   io.Closer
	fileLoggerRotation = FileLogRotationConfig{
		MaxSizeMB:  config.SidecarRotationMaxSizeMB,
		MaxBackups: config.SidecarRotationMaxBackups,
		MaxAgeDays: config.SidecarRotationMaxAgeDays,
		Compress:   new(true),
	}
)

// ConfigureAnthropicFileLogger installs rotation settings for anthropic.jsonl
// before the first Anthropic event is emitted. Later calls are ignored
// because slog handlers bind their writer path and lumberjack settings at
// construction.
func ConfigureAnthropicFileLogger(rotation FileLogRotationConfig) {
	fileLoggerMu.Lock()
	defer fileLoggerMu.Unlock()
	if fileLogger != nil {
		return
	}
	fileLoggerRotation = normalizeAnthropicLogRotation(rotation)
}

// LogPath returns the JSONL file the anthropic package
// double-writes its events to. Honors $CLYDE_ANTHROPIC_LOG_PATH for
// tests; otherwise lives next to the unified clyde log under
// $XDG_STATE_HOME/clyde/anthropic.jsonl.
func LogPath() string {
	if p := os.Getenv("CLYDE_ANTHROPIC_LOG_PATH"); p != "" {
		return p
	}
	return filepath.Join(config.DefaultStateDir(), "anthropic.jsonl")
}

func dedicatedLogger() *slog.Logger {
	fileLoggerMu.Lock()
	defer fileLoggerMu.Unlock()
	fileLoggerOnce.Do(func() {
		path := LogPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return
		}
		handler := gklog.FileJSON(path, slog.LevelDebug, fileLoggerRotation.toGKLog())
		if closer, ok := handler.(io.Closer); ok {
			fileLoggerCloser = closer
		}
		fileLogger = slog.New(handler)
	})
	return fileLogger
}

// responseEvent is the typed payload for every /v1/messages response
// log line (success, ratelimit, upstream error, post failure). The
// only place that materializes []any for slog is toSlogAttrs(); call
// sites build a struct literal and hand it to logResponse, which
// keeps the variadic shape contained to a single helper.
//
// Optional fields use the zero value as the "omit" sentinel:
// RetryAfter/Err empty strings are dropped, and Status==0 means
// the response never came back (post_failed). Response bodies are never
// logged; full request and response bodies (including error bodies) live in the
// SQLite capture store, so the event carries only BodyBytes.
type responseEvent struct {
	Subcomponent string
	Model        string
	Status       int
	RequestID    string
	BodyBytes    int
	DurationMs   int64
	RateLimits   []rateLimitAttr
	RetryAfter   string
	Err          string
}

func (e responseEvent) toSlogAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 7+len(e.RateLimits))
	if e.Subcomponent != "" {
		attrs = append(attrs, slog.String("subcomponent", e.Subcomponent))
	}
	if e.Model != "" {
		attrs = append(attrs, slog.String("model", e.Model))
	}
	if e.Status != 0 {
		attrs = append(attrs, slog.Int("status", e.Status))
	}
	if e.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", e.RequestID))
	}
	attrs = append(attrs, slog.Int("body_bytes", e.BodyBytes))
	attrs = append(attrs, slog.Int64("duration_ms", e.DurationMs))
	for _, r := range e.RateLimits {
		attrs = append(attrs, slog.String(r.Name, r.Value))
	}
	if e.RetryAfter != "" {
		attrs = append(attrs, slog.String("retry_after", e.RetryAfter))
	}
	if e.Err != "" {
		attrs = append(attrs, slog.String("err", e.Err))
	}
	return attrs
}

// logResponse writes the event to both [slog.Default]() and the
// dedicated anthropic JSONL file. The dedicated file is best effort;
// a missing log dir never blocks API traffic. The concern attr is
// injected at the wrapper boundary so the per-concern router routes
// every Anthropic response event into the request file.
func logResponse(ctx context.Context, level slog.Level, event string, e responseEvent) {
	attrs := append([]slog.Attr{slog.String("concern", "adapter.providers.anthropic.request")}, e.toSlogAttrs()...)
	slog.Default().LogAttrs(ctx, level, event, attrs...)
	if l := dedicatedLogger(); l != nil {
		l.LogAttrs(ctx, level, event, attrs...)
	}
}
