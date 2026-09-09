package cursorstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/mattn/go-sqlite3"
)

// Availability can recover without a content stamp change. Malformed JSON,
// invalid database contents, and missing schema columns remain stamped outcomes.
func isReadAvailabilityError(err error) bool {
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return true
	}
	var sqliteError sqlite3.Error
	if !errors.As(err, &sqliteError) {
		return false
	}
	switch sqliteError.Code {
	case sqlite3.ErrBusy, sqlite3.ErrLocked, sqlite3.ErrPerm, sqlite3.ErrIoErr,
		sqlite3.ErrCantOpen, sqlite3.ErrNomem, sqlite3.ErrFull, sqlite3.ErrProtocol, sqlite3.ErrInterrupt:
		return true
	default:
		return false
	}
}

type cachedReadDiagnostic struct {
	message string
	level   slog.Level
	fields  string
	scope   string
}

type cachedReadDiagnostics struct {
	previous map[cachedReadDiagnostic]bool
}

type cachedReadAttempt struct {
	mu       sync.Mutex
	previous map[cachedReadDiagnostic]bool
	current  map[cachedReadDiagnostic]bool
}

type cachedReadLoggerKey uint8

func (diagnostics *cachedReadDiagnostics) start(ctx context.Context, changed bool) (context.Context, *cachedReadAttempt) {
	if changed {
		diagnostics.previous = nil
	}
	attempt := &cachedReadAttempt{mu: sync.Mutex{}, previous: diagnostics.previous, current: make(map[cachedReadDiagnostic]bool)}
	logger := slog.New(&cachedReadHandler{next: slog.Default().Handler(), attempt: attempt, scope: ""})
	return context.WithValue(ctx, cachedReadLoggerKey(0), logger), attempt
}

func (diagnostics *cachedReadDiagnostics) finish(attempt *cachedReadAttempt, err error) {
	diagnostics.previous = nil
	if err != nil {
		diagnostics.previous = attempt.current
	}
}

func discoveryReadLogger(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if logger, ok := ctx.Value(cachedReadLoggerKey(0)).(*slog.Logger); ok {
			return logger
		}
	}
	return slog.Default()
}

// cachedReadHandler only wraps a cache refresh. Outside that read the original
// logger remains in use; records are forwarded without changing their fields.
type cachedReadHandler struct {
	next    slog.Handler
	attempt *cachedReadAttempt
	scope   string
}

func (handler *cachedReadHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.next.Enabled(ctx, level)
}

func (handler *cachedReadHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level < slog.LevelWarn {
		return handler.writeRecord(ctx, record)
	}
	var fields strings.Builder
	record.Attrs(func(attr slog.Attr) bool { appendDiagnosticAttr(&fields, attr); return true })
	key := cachedReadDiagnostic{message: record.Message, level: record.Level, fields: fields.String(), scope: handler.scope}
	handler.attempt.mu.Lock()
	duplicate := handler.attempt.previous[key] || handler.attempt.current[key]
	handler.attempt.current[key] = true
	handler.attempt.mu.Unlock()
	if duplicate {
		return nil
	}
	return handler.writeRecord(ctx, record)
}

func (handler *cachedReadHandler) writeRecord(ctx context.Context, record slog.Record) error {
	if err := handler.next.Handle(ctx, record); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.diagnostic_write_failed", "concern", concern, "message", record.Message, "err", err)
		return fmt.Errorf("write cursor cached-read diagnostic %q: %w", record.Message, err)
	}
	return nil
}

func (handler *cachedReadHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var scope strings.Builder
	scope.WriteString(handler.scope)
	for _, attr := range attrs {
		appendDiagnosticAttr(&scope, attr)
	}
	return &cachedReadHandler{next: handler.next.WithAttrs(attrs), attempt: handler.attempt, scope: scope.String()}
}

func (handler *cachedReadHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return handler
	}
	return &cachedReadHandler{next: handler.next.WithGroup(name), attempt: handler.attempt, scope: handler.scope + "\x00group:" + name}
}

func appendDiagnosticAttr(builder *strings.Builder, attr slog.Attr) {
	builder.WriteString(attr.Key)
	builder.WriteByte(0)
	builder.WriteString(attr.Value.Resolve().String())
	builder.WriteByte(0)
}
