// Package slogger is the clyde-wide structured logging facade.
//
// It is a thin wrapper around goodkind.io/gklog (the cross-repo logging
// package). Request scoped loggers on context use goodkind.io/gklog
// (WithLogger, LoggerFromContext). Every call site uses Go's
// standard log/slog package directly; this package only handles initialization
// (Setup).
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
	defaultBaseSubdir = "clyde"
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

// Setup initializes the global slog logger via gklog. It writes
// JSONL to a process-specific path under $XDG_STATE_HOME/clyde
// (or [logging.paths] when configured). Stdout logging is disabled
// so command output remains machine-friendly
// for CLI callers. Call once at process start before emitting any events;
// otherwise slog.Default falls back to a stderr text handler.
//
// concernRotationOverrides supplies non-default rotation budgets for specific
// concern paths; callers build it from typed adapter sub-blocks (e.g.
// [adapter.wire_capture.rotation]) and pass an empty map when no overrides
// apply.
//
// Returns an io.Closer that the caller must Close on shutdown so the
// rotating file handles flush. closer.Close() is safe to call once.
func Setup(cfg config.LoggingConfig, role ProcessRole, concernRotationOverrides ConcernRotationOverrides) (io.Closer, error) {
	level := strings.ToLower(strings.TrimSpace(cfg.Level))
	if level == "" {
		level = "info"
	}
	switch level {
	case "debug", "info", "warn", "error":
	default:
		slog.Warn("slogger.setup.invalid_level",
			"component", "slogger",
			"level", level,
		)
		return nopCloser{}, fmt.Errorf("slogger: logging.level required, must be one of debug|info|warn|error, got %q", level)
	}

	path := defaultPath(cfg, role)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("slogger.setup.mkdir_failed",
			"component", "slogger",
			"path", filepath.Dir(path),
			"err", err,
		)
		return nopCloser{}, fmt.Errorf("slogger: mkdir %s: %w", filepath.Dir(path), err)
	}
	concernRoot := defaultConcernRoot(cfg, role)
	rotationEnabled := true
	if cfg.Rotation.Enabled != nil {
		rotationEnabled = *cfg.Rotation.Enabled
	}
	if !rotationEnabled {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			slog.Warn("slogger.setup.open_json_log_failed",
				"component", "slogger",
				"path", path,
				"err", err,
			)
			return nopCloser{}, fmt.Errorf("slogger: open json log file %s: %w", path, err)
		}
		lockedFile := gklog.NewLockedWriteCloser(path, file)
		handlers := []slog.Handler{slog.NewJSONHandler(lockedFile, &slog.HandlerOptions{Level: parseJSONMinLevel(level)})}
		handlers = append(handlers, concernHandlers(concernRoot, parseJSONMinLevel(level), gklog.RotationConfig{}, concernRotationOverrides)...)
		router := buildTranscriptRouter(cfg.Transcript, concernRoot)
		if router != nil {
			handlers = append(handlers, router)
		}
		logger := slog.New(newCorrelationHandler(gklog.NewTeeHandler(handlers...)))
		slog.SetDefault(logger.With("build", version.String()))
		if router == nil {
			return lockedFile, nil
		}
		return &transcriptRouterCloser{router: router, inner: lockedFile}, nil
	}
	// stdout is reserved for command output (so CLI subcommands like
	// `clyde compact clone-for-test --print-name` produce machine-
	// parseable single-line output). slog goes to the rotated JSONL
	// file at the resolved process path; tail that file for
	// live diagnostics.
	compress := cfg.Rotation.Compress
	if compress == nil {
		compress = new(true)
	}
	handlers := []slog.Handler{
		gklog.FileJSON(path, parseJSONMinLevel(level), gklog.RotationConfig{
			MaxSizeMB:  cfg.Rotation.MaxSizeMB,
			MaxBackups: cfg.Rotation.MaxBackups,
			MaxAgeDays: cfg.Rotation.MaxAgeDays,
			Compress:   compress,
		}),
	}
	handlers = append(handlers, concernHandlers(concernRoot, parseJSONMinLevel(level), gklog.RotationConfig{
		MaxSizeMB:  cfg.Rotation.MaxSizeMB,
		MaxBackups: cfg.Rotation.MaxBackups,
		MaxAgeDays: cfg.Rotation.MaxAgeDays,
		Compress:   compress,
	}, concernRotationOverrides)...)
	router := buildTranscriptRouter(cfg.Transcript, concernRoot)
	if router != nil {
		handlers = append(handlers, router)
	}
	logger, closer := gklog.New(gklog.Config{
		BuildVersion: version.String(),
		Handlers:     []slog.Handler{newCorrelationHandler(gklog.NewTeeHandler(handlers...))},
	})
	slog.SetDefault(logger)
	if router == nil {
		return closer, nil
	}
	return &transcriptRouterCloser{router: router, inner: closer}, nil
}

// buildTranscriptRouter returns a configured router, or nil when the feature
// is off (disabled or missing retention bounds). The router writes under
// <concernRoot>/chats/.
func buildTranscriptRouter(cfg config.LoggingTranscript, concernRoot string) *TranscriptRouter {
	if !cfg.IsEnabled() {
		return nil
	}
	mode := TranscriptMode(cfg.Mode)
	if mode != TranscriptModeRaw {
		mode = TranscriptModeSummary
	}
	return NewTranscriptRouter(TranscriptRouterConfig{
		Root: filepath.Join(concernRoot, "chats"),
		Mode: mode,
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

func parseJSONMinLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelDebug
	}
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
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), defaultBaseSubdir, fileForRole(role))
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, defaultBaseSubdir, fileForRole(role))
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
