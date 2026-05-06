package adapter

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/slogger"
)

// logHTTPRequestDebug captures inbound request shape (headers, body, body
// metadata) at debug level when adapter body logging is enabled. The adapter
// boundary in handle.go calls this before invoking the handler so every
// route picks up the same diagnostic without per-handler plumbing.
func (s *Server) logHTTPRequestDebug(ctx context.Context, r *http.Request) {
	body, readErr := readAndRestoreBody(r)
	bodyLogging := s.bodyLogging()
	bodyLimit := bodyLogging.MaxKB * 1024
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("remote_addr", r.RemoteAddr),
		slog.String("user_agent", r.UserAgent()),
		slog.Any("headers", redactedHeaders(r.Header)),
		slog.Int("body_bytes", len(body)),
	}
	switch bodyLogging.Mode {
	case "raw":
		raw, truncated := truncateBody(body, bodyLimit)
		if raw != "" {
			attrs = append(attrs, slog.String("body", raw))
		}
		if b64 := encodeBodyB64(body, bodyLimit); b64 != "" {
			attrs = append(attrs, slog.String("body_b64", b64))
		}
		if truncated {
			attrs = append(attrs, slog.Bool("body_truncated", true))
		}
	case "whitelist":
		raw, truncated := truncateBody(body, bodyLimit)
		if raw != "" {
			attrs = append(attrs, slog.String("body", raw))
		}
		if truncated {
			attrs = append(attrs, slog.Bool("body_truncated", true))
		}
	}
	if readErr != nil {
		attrs = append(attrs, slog.String("body_read_error", readErr.Error()))
	}
	attrs = append(attrs, correlation.AttrsFromContext(ctx)...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterHTTPRaw).LogAttrs(ctx, slog.LevelDebug, "adapter.request.raw", attrs...)
}

func (s *Server) bodyLogging() config.LoggingBody {
	if s == nil || s.runtimeLogging == nil {
		return normalizeRuntimeLoggingBody(config.LoggingBody{})
	}
	return s.runtimeLogging.Body()
}

func readAndRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}
