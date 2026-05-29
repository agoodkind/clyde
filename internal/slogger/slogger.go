// Package slogger is the clyde-wide structured logging facade.
//
// It is a thin wrapper around goodkind.io/gklog (the cross-repo logging
// package). Request scoped loggers on context use goodkind.io/gklog
// (WithLogger, LoggerFromContext). Every call site uses Go's
// standard log/slog package directly; this package only handles initialization
// (SetupWithPolicy).
//
// The standard is non-negotiable: every operation in the codebase MUST
// emit at least one slog event. Free-form [fmt.Println] / [log.Printf] are
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
	"goodkind.io/gklog"
	"goodkind.io/gklog/correlation"
	"goodkind.io/gklog/version"
)

const (
	envOverride       = "CLYDE_SLOG_PATH"
	defaultCLIFile    = "clyde-cli.jsonl"
	defaultDaemonFile = "clyde-daemon.jsonl"
	concernAttr       = "concern"
)

// ProcessRole identifies which process family is initializing slog.
type ProcessRole string

const (
	// ProcessRoleCLI identifies a short-lived CLI invocation.
	ProcessRoleCLI ProcessRole = "cli"
	// ProcessRoleDaemon identifies the long-lived daemon process.
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
//
// SetupWithPolicy no longer runs the file-cleanup walker. The walker now runs
// only inside the daemon, on the periodic loop, via RunCleanupOnce. CLI and
// CLI invocations must not block on filesystem scans during process start.
func SetupWithPolicy(policy SetupPolicy) (io.Closer, error) {
	if err := validateConcernPolicyNames(policy.ConcernPolicies); err != nil {
		return nopCloser{Closed: false}, err
	}
	return setupPolicyLogger(policy)
}

func setupPolicyLogger(policy SetupPolicy) (io.Closer, error) {
	if !policy.ProcessSink.Enabled {
		handlers := appendSharedPolicyHandlers(nil, policy, policy.ProcessSink.Rotation)
		return buildPolicyAsyncLogger(handlers), nil
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
		handlers = appendSharedPolicyHandlers(handlers, policy, disabledRotation)
		closer := buildPolicyAsyncLogger(handlers)
		return newMultiCloser(closer, lockedFile), nil
	}
	handlers := []slog.Handler{
		gklog.FileJSON(path, policy.Level, rotationConfig(policy.ProcessSink.Rotation)),
	}
	handlers = appendSharedPolicyHandlers(handlers, policy, policy.ProcessSink.Rotation)
	return buildPolicyAsyncLogger(handlers), nil
}

func appendSharedPolicyHandlers(handlers []slog.Handler, policy SetupPolicy, rotation RotationPolicy) []slog.Handler {
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

func buildPolicyAsyncLogger(handlers []slog.Handler) io.Closer {
	if len(handlers) == 0 {
		handlers = append(handlers, slog.DiscardHandler)
	}
	handlerCloser := handlersCloser(handlers)
	rootHandler := correlation.SlogHandler(
		newAsyncHandler(gklog.NewTeeHandler(handlers...), handlerCloser),
		correlation.HandlerOptions{
			Strict:   false,
			Required: nil,
		},
	)
	logger := slog.New(rootHandler).With("build", version.String())
	slog.SetDefault(logger)
	closer, ok := rootHandler.(io.Closer)
	if !ok {
		return nopCloser{Closed: false}
	}
	return closer
}

// buildTranscriptRouter returns a configured router, or nil when the feature
// is off (disabled or missing retention bounds). The router writes under
// <concernRoot>/chats/.
func buildTranscriptRouter(policy TranscriptPolicy, concernRoot string) *TranscriptRouter {
	if !policy.Enabled {
		return nil
	}
	return NewTranscriptRouter(TranscriptRouterConfig{
		Root: filepath.Join(concernRoot, "chats"), PoolCap: 0,

		// WithConcern is part of Clyde's typed adapter surface.
		Now: nil,
	})
}

// WithConcern is part of Clyde's typed adapter surface.
func WithConcern(logger *slog.Logger, concern string) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	if concern = strings.TrimSpace(concern); concern == "" {
		return logger
	}
	return logger.With(concernAttr, concern)
}

// For is part of Clyde's typed adapter surface.
func For(concern string) *slog.Logger {
	return WithConcern(slog.Default(), concern)
}

// ConcernLogger is a package-level safe concern logger.
//
// It intentionally resolves [slog.Default] at each call instead of retaining a
// *[slog.Logger] captured during package init. Clyde initializes logging after
// packages are loaded, so package-level `slogger.For(...)` variables would bind
// to Go's bootstrap text logger and corrupt JSON logs after setup.
type ConcernLogger string

// Concern is part of Clyde's typed adapter surface.
func Concern(concern string) ConcernLogger {
	return ConcernLogger(concern)
}

// Logger is part of Clyde's typed adapter surface.
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
	if role == ProcessRoleCLI && cfg.Paths.CLI != "" {
		return cfg.Paths.CLI
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
	return defaultCLIFile
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

func newConcernFilterHandler(concern string, handler slog.Handler) slog.Handler {
	return &concernFilterHandler{concern: concern, handler: handler, attrs: nil}
}

func (h *concernFilterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *concernFilterHandler) Handle(ctx context.Context, record slog.Record) error {
	if !h.matches(record) {
		return nil
	}
	err := h.handler.Handle(ctx, record)
	if err != nil {
		// Use stderr rather than slog to avoid recursing into the
		// same handler chain when the inner handler reports a write
		// error.
		fmt.Fprintln(os.Stderr, "slogger.concern_filter.handle_failed:", err.Error())
		slog.WarnContext(ctx, "slogger.concern_filter.handle_failed", "concern", h.concern, "err", err)
		return fmt.Errorf("handle concern-filtered slog record: %w", err)
	}
	return nil
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
		if err := closer.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "slogger.concern_filter.close_failed:", err.Error())
			slog.Warn("slogger.concern_filter.close_failed", "concern", h.concern, "err", err)
			return fmt.Errorf("close concern-filtered slog handler: %w", err)
		}
	}
	return nil
}

// matches reports whether record carries an explicit concern
// attribute that selects this handler. Every slog call site in the
// repository emits the concern attr explicitly; the historical
// message-prefix router has been removed.
func (h *concernFilterHandler) matches(record slog.Record) bool {
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
