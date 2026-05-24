// Package slogger is the clyde-wide structured logging facade.
//
// It is a thin wrapper around goodkind.io/gklog (the cross-repo logging
// package). Request scoped loggers on context use goodkind.io/gklog
// (WithLogger, LoggerFromContext). Every call site uses Go's
// standard log/slog package directly; this package only handles initialization
// (SetupWithPolicy).
//
// The standard is non-negotiable: every operation in the codebase MUST
// emit at least one slog event. Free-form fmt.Println / log.Printf are
// rejected by `make slog-audit`. See AGENTS.md for the full spec.
package slogger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/gklog"
	"goodkind.io/gklog/version"
)

const (
	envOverride       = "CLYDE_SLOG_PATH"
	defaultTUIFile    = "clyde-tui.jsonl"
	defaultDaemonFile = "clyde-daemon.jsonl"
	concernAttr       = "concern"
)

// ProcessRole identifies which process family is initializing slog.
type ProcessRole string

const (
	ProcessRoleTUI    ProcessRole = "tui"
	ProcessRoleDaemon ProcessRole = "daemon"
)

// DefaultProcessPath resolves the process-aware JSONL path used by policy setup.
func DefaultProcessPath(cfg config.LoggingConfig, role ProcessRole) string {
	return defaultPath(cfg, role)
}

// DefaultConcernRoot resolves the concern log root used by policy setup.
func DefaultConcernRoot(cfg config.LoggingConfig, role ProcessRole) string {
	return defaultConcernRoot(cfg, role)
}

// SetupWithPolicy initializes slog from a resolved typed policy. This is the
// policy-driven path for config/logpolicy resolvers.
func SetupWithPolicy(policy SetupPolicy) (io.Closer, error) {
	if err := applyCleanupPolicy(policy.CleanupPolicy); err != nil {
		return nopCloser{Closed: false}, err
	}
	if err := validateConcernPolicyNames(policy.ConcernPolicies); err != nil {
		return nopCloser{Closed: false}, err
	}
	appendSharedHandlers := func(handlers []slog.Handler, rotation RotationPolicy) []slog.Handler {
		handlers = append(handlers, concernHandlers(policy.ConcernRoot, policy.Level, rotation, policy.ConcernPolicies)...)
		if captureHandler := buildMITMCaptureIndexHandler(policy.MITMCapturePolicy, policy.Level); captureHandler != nil {
			handlers = append(handlers, captureHandler)
		}
		if inventoryHandler := buildInventoryIndexHandler(policy.InventoryPolicy, policy.ConcernRoot, policy.Level); inventoryHandler != nil {
			handlers = append(handlers, inventoryHandler)
		}
		if router := buildTranscriptRouter(policy.TranscriptPolicy, policy.ConcernRoot); router != nil {
			handlers = append(handlers, router)
		}
		return handlers
	}
	buildAsyncLogger := func(handlers []slog.Handler) (io.Closer, error) {
		if len(handlers) == 0 {
			handlers = append(handlers, slog.DiscardHandler)
		}
		handlerCloser := handlersCloser(handlers)
		rootHandler := newCorrelationHandler(newAsyncHandler(gklog.NewTeeHandler(handlers...), handlerCloser))
		logger := slog.New(rootHandler).With("build", version.String())
		slog.SetDefault(logger)
		closer, ok := rootHandler.(io.Closer)
		if !ok {
			return nopCloser{Closed: false}, nil
		}
		return closer, nil
	}
	if !policy.ProcessSink.Enabled {
		handlers := appendSharedHandlers(nil, policy.ProcessSink.Rotation)
		return buildAsyncLogger(handlers)
	}
	path := policy.ProcessSink.Path
	if strings.TrimSpace(path) == "" {
		slog.Warn("slogger.setup.empty_process_path", "component", "slogger")
		return nopCloser{Closed: false}, fmt.Errorf("slogger: process log path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("slogger.setup.mkdir_failed",
			"component", "slogger",
			"path", filepath.Dir(path),
			"err", err,
		)
		return nopCloser{Closed: false}, fmt.Errorf("slogger: mkdir %s: %w", filepath.Dir(path), err)
	}
	if !policy.ProcessSink.Rotation.Enabled {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			slog.Warn("slogger.setup.open_json_log_failed",
				"component", "slogger",
				"path", path,
				"err", err,
			)
			return nopCloser{Closed: false}, fmt.Errorf("slogger: open json log file %s: %w", path, err)
		}
		lockedFile := gklog.NewLockedWriteCloser(path, file)
		handlers := []slog.Handler{slog.NewJSONHandler(lockedFile, &slog.HandlerOptions{Level: policy.Level})}
		disabledRotation := RotationPolicy{
			Enabled:    false,
			MaxSizeMB:  0,
			MaxBackups: 0,
			MaxAgeDays: 0,
			Compress:   nil,
		}
		handlers = appendSharedHandlers(handlers, disabledRotation)
		closer, err := buildAsyncLogger(handlers)
		if err != nil {
			_ = lockedFile.Close()
			return nopCloser{Closed: false}, err
		}
		return newMultiCloser(closer, lockedFile), nil
	}
	handlers := []slog.Handler{
		gklog.FileJSON(path, policy.Level, rotationConfig(policy.ProcessSink.Rotation)),
	}
	handlers = appendSharedHandlers(handlers, policy.ProcessSink.Rotation)
	return buildAsyncLogger(handlers)
}

// buildTranscriptRouter returns a configured router, or nil when the feature
// is off (disabled or missing retention bounds). The router writes under
// <concernRoot>/chats/.
func buildTranscriptRouter(policy TranscriptPolicy, concernRoot string) *TranscriptRouter {
	if !policy.Enabled {
		return nil
	}
	return NewTranscriptRouter(TranscriptRouterConfig{
		Root: filepath.Join(concernRoot, "chats"),
	})
}

func WithConcern(logger *slog.Logger, concern string) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	if concern = strings.TrimSpace(concern); concern == "" {
		return logger
	}
	return logger.With(concernAttr, concern)
}

func For(concern string) *slog.Logger {
	return WithConcern(slog.Default(), concern)
}

// ConcernLogger is a package-level safe concern logger.
//
// It intentionally resolves slog.Default at each call instead of retaining a
// *slog.Logger captured during package init. Clyde initializes logging after
// packages are loaded, so package-level `slogger.For(...)` variables would bind
// to Go's bootstrap text logger and corrupt JSON logs after setup.
type ConcernLogger string

func Concern(concern string) ConcernLogger {
	return ConcernLogger(concern)
}

func (l ConcernLogger) Logger() *slog.Logger {
	return For(string(l))
}

// defaultPath resolves the process-aware JSONL path. Honors the env
// override for tests. Operators may set [logging.paths] to override
// the per-role defaults.
func defaultPath(cfg config.LoggingConfig, role ProcessRole) string {
	if p := os.Getenv(envOverride); p != "" {
		return p
	}
	if role == ProcessRoleDaemon && cfg.Paths.Daemon != "" {
		return cfg.Paths.Daemon
	}
	if role == ProcessRoleTUI && cfg.Paths.TUI != "" {
		return cfg.Paths.TUI
	}
	return filepath.Join(config.DefaultStateDir(), fileForRole(role))
}

func defaultConcernRoot(cfg config.LoggingConfig, role ProcessRole) string {
	return filepath.Join(filepath.Dir(defaultPath(cfg, role)), "logs")
}

func fileForRole(role ProcessRole) string {
	if role == ProcessRoleDaemon {
		return defaultDaemonFile
	}
	return defaultTUIFile
}

type nopCloser struct {
	Closed bool
}

func (nopCloser) Close() error { return nil }

type concernFilterHandler struct {
	concern string
	attrs   []slog.Attr
	handler slog.Handler
}

type correlationHandler struct {
	attrs   []slog.Attr
	handler slog.Handler
}

func newCorrelationHandler(handler slog.Handler) slog.Handler {
	return &correlationHandler{handler: handler}
}

func (h *correlationHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *correlationHandler) Handle(ctx context.Context, record slog.Record) error {
	corrAttrs := correlation.AttrsFromContext(ctx)
	if len(corrAttrs) == 0 {
		return h.handler.Handle(ctx, record)
	}
	existing := attrKeySet(h.attrs, record)
	var missing []slog.Attr
	for _, attr := range corrAttrs {
		if !existing[attr.Key] {
			missing = append(missing, attr)
		}
	}
	if len(missing) == 0 {
		return h.handler.Handle(ctx, record)
	}
	next := record.Clone()
	next.AddAttrs(missing...)
	return h.handler.Handle(ctx, next)
}

func (h *correlationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &correlationHandler{
		attrs:   append(append([]slog.Attr(nil), h.attrs...), attrs...),
		handler: h.handler.WithAttrs(attrs),
	}
}

func (h *correlationHandler) WithGroup(name string) slog.Handler {
	return &correlationHandler{
		attrs:   append([]slog.Attr(nil), h.attrs...),
		handler: h.handler.WithGroup(name),
	}
}

func (h *correlationHandler) Close() error {
	if closer, ok := h.handler.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func attrKeySet(handlerAttrs []slog.Attr, record slog.Record) map[string]bool {
	keys := make(map[string]bool, len(handlerAttrs)+record.NumAttrs())
	for _, attr := range handlerAttrs {
		keys[attr.Key] = true
	}
	record.Attrs(func(attr slog.Attr) bool {
		keys[attr.Key] = true
		return true
	})
	return keys
}

func newConcernFilterHandler(concern string, handler slog.Handler) slog.Handler {
	return &concernFilterHandler{concern: concern, handler: handler}
}

func (h *concernFilterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *concernFilterHandler) Handle(ctx context.Context, record slog.Record) error {
	if !h.matches(record) {
		return nil
	}
	return h.handler.Handle(ctx, record)
}

func (h *concernFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &concernFilterHandler{
		concern: h.concern,
		attrs:   append(append([]slog.Attr(nil), h.attrs...), attrs...),
		handler: h.handler.WithAttrs(attrs),
	}
	return next
}

func (h *concernFilterHandler) WithGroup(name string) slog.Handler {
	return &concernFilterHandler{
		concern: h.concern,
		attrs:   append([]slog.Attr(nil), h.attrs...),
		handler: h.handler.WithGroup(name),
	}
}

func (h *concernFilterHandler) Close() error {
	if closer, ok := h.handler.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (h *concernFilterHandler) matches(record slog.Record) bool {
	if concernForEvent(record.Message) == h.concern {
		return true
	}
	for _, attr := range h.attrs {
		if attr.Key == concernAttr && attr.Value.String() == h.concern {
			return true
		}
	}
	matched := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == concernAttr && attr.Value.String() == h.concern {
			matched = true
			return false
		}
		return true
	})
	return matched
}
