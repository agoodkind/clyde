// Package anthropic implements Anthropic wire models and helpers.
package anthropic

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

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
	fileLoggerOnce sync.Once
	fileLogger     *slog.Logger
)

// AnthropicLogPath returns the JSONL file the anthropic package
// double-writes its events to. Honors $CLYDE_ANTHROPIC_LOG_PATH for
// tests; otherwise lives next to the unified clyde log under
// $XDG_STATE_HOME/clyde/anthropic.jsonl.
func AnthropicLogPath() string {
	if p := os.Getenv("CLYDE_ANTHROPIC_LOG_PATH"); p != "" {
		return p
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "clyde", "anthropic.jsonl")
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "clyde", "anthropic.jsonl")
}

func dedicatedLogger() *slog.Logger {
	fileLoggerOnce.Do(func() {
		path := AnthropicLogPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		fileLogger = slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}))
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
// RetryAfter/Body/Err empty strings are dropped, and Status==0 means
// the response never came back (post_failed).
type responseEvent struct {
	Subcomponent string
	Model        string
	Status       int
	RequestID    string
	BodyBytes    int
	DurationMs   int64
	RateLimits   []rateLimitAttr
	RetryAfter   string
	Body         string
	Err          string
}

func (e responseEvent) toSlogAttrs() []any {
	attrs := make([]any, 0, 14+2*len(e.RateLimits))
	if e.Subcomponent != "" {
		attrs = append(attrs, "subcomponent", e.Subcomponent)
	}
	if e.Model != "" {
		attrs = append(attrs, "model", e.Model)
	}
	if e.Status != 0 {
		attrs = append(attrs, "status", e.Status)
	}
	if e.RequestID != "" {
		attrs = append(attrs, "request_id", e.RequestID)
	}
	attrs = append(attrs, "body_bytes", e.BodyBytes)
	attrs = append(attrs, "duration_ms", e.DurationMs)
	for _, r := range e.RateLimits {
		attrs = append(attrs, r.Name, r.Value)
	}
	if e.RetryAfter != "" {
		attrs = append(attrs, "retry_after", e.RetryAfter)
	}
	if e.Body != "" {
		attrs = append(attrs, "body", e.Body)
	}
	if e.Err != "" {
		attrs = append(attrs, "err", e.Err)
	}
	return attrs
}

// logResponse writes the event to both slog.Default() and the
// dedicated anthropic JSONL file. The dedicated file is best effort;
// a missing log dir never blocks API traffic.
func logResponse(level slog.Level, event string, e responseEvent) {
	attrs := e.toSlogAttrs()
	slog.Default().Log(context.Background(), level, event, attrs...)
	if l := dedicatedLogger(); l != nil {
		l.Log(context.Background(), level, event, attrs...)
	}
}
