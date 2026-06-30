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
	// routerFallbackConcern is the concern the gklog router assigns to records
	// that carry no concern attr. Its PathForConcern mapping returns the empty
	// string so those records never reach a per-concern file.
	routerFallbackConcern = "unrouted"
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
	handlers = append(handlers, buildConcernRouter(policy, rotation))
	if inventoryHandler := buildInventoryIndexHandler(policy.InventoryPolicy, policy.ConcernRoot, policy.Level); inventoryHandler != nil {
		handlers = append(handlers, inventoryHandler)
	}
	if router := buildTranscriptRouter(policy.TranscriptPolicy, policy.ConcernRoot); router != nil {
		handlers = append(handlers, router)
	}
	return handlers
}

// buildConcernRouter installs the gklog per-concern file router. The router
// reads the concern from the "concern" record attribute that every clyde call
// site tags via [Concern] / [WithConcern], maps it to a JSONL file under the
// concern root via [ConcernRelPath], and applies any per-concern level override
// from the policy. Records that carry no concern attr resolve to the
// "unrouted" fallback, which [routerPathForConcern] keeps out of every concern
// file so they land only on the process log (a sibling handler in the tee).
// The router's combined sink is discarded here because the process log is its
// own handler in the tee.
func buildConcernRouter(policy SetupPolicy, rotation RotationPolicy) slog.Handler {
	routerRotation := gklog.RotationConfig{}
	if rotation.Enabled {
		routerRotation = rotationConfig(rotation)
	}
	// The router's own Enabled gate must clear the lowest per-concern level so a
	// concern override below the process-wide level still reaches Handle, where
	// the per-concern child handler applies the precise level. Gating the router
	// at policy.Level alone would let slog drop those records before routing.
	routerLevel := routerMinLevel(policy.Level, policy.ConcernPolicies)
	return gklog.NewRouter(policy.ConcernRoot, routerLevel, slog.DiscardHandler, gklog.RouterOptions{
		FallbackConcern: routerFallbackConcern,
		Rotation:        routerRotation,
		ConcernAttr:     concernAttr,
		PathForConcern:  routerPathForConcern(policy.ConcernPolicies),
		LevelForConcern: routerLevelForConcern(policy.Level, policy.ConcernPolicies),
	})
}

// routerMinLevel returns the lowest of the process-wide level and every
// explicit per-concern level override, which is the level the router's own
// Enabled gate must use so no concern's records are dropped before routing.
func routerMinLevel(defaultLevel slog.Level, policies map[string]ConcernPolicy) slog.Level {
	minLevel := defaultLevel
	for _, policy := range policies {
		if policy.Level != nil && *policy.Level < minLevel {
			minLevel = *policy.Level
		}
	}
	return minLevel
}

// routerPathForConcern returns the file router's concern-to-path mapper. The
// fallback concern and any concern whose policy is explicitly disabled return
// the empty string, which keeps those records out of per-concern files while
// still letting the process-log sibling handler record them.
func routerPathForConcern(policies map[string]ConcernPolicy) func(string) string {
	return func(concern string) string {
		if concern == routerFallbackConcern {
			return ""
		}
		if policy, ok := policies[concern]; ok && policy.Enabled != nil && !*policy.Enabled {
			return ""
		}
		return ConcernRelPath(concern)
	}
}

// routerLevelForConcern returns the file router's per-concern level resolver.
// A concern with an explicit level override uses that level; every other
// concern uses the process-wide default level.
func routerLevelForConcern(defaultLevel slog.Level, policies map[string]ConcernPolicy) func(string) slog.Level {
	return func(concern string) slog.Level {
		if policy, ok := policies[concern]; ok && policy.Level != nil {
			return *policy.Level
		}
		return defaultLevel
	}
}

func buildPolicyAsyncLogger(handlers []slog.Handler) io.Closer {
	if len(handlers) == 0 {
		handlers = append(handlers, slog.DiscardHandler)
	}
	// Each sink gets its own async drain goroutine behind the tee, rather than a
	// single drain wrapping the whole tee. A rotation or a slow write on one log
	// file then runs on that file's own goroutine and never blocks the producers
	// or the other sinks, so log rotation is off every producing hot path.
	asyncChildren := make([]slog.Handler, len(handlers))
	closers := make([]io.Closer, 0, len(handlers))
	for i, handler := range handlers {
		async := newAsyncHandler(handler, sinkCloser(handler))
		asyncChildren[i] = async
		closers = append(closers, async)
	}
	rootHandler := correlation.SlogHandler(
		gklog.NewTeeHandler(asyncChildren...),
		correlation.HandlerOptions{
			Strict:   false,
			Required: nil,
		},
	)
	logger := slog.New(rootHandler).With("build", version.String())
	slog.SetDefault(logger)
	return newMultiCloser(closers...)
}

// sinkCloser returns handler as an [io.Closer] when it owns OS resources that
// must be flushed and released on shutdown, or nil when it does not.
func sinkCloser(handler slog.Handler) io.Closer {
	if closer, ok := handler.(io.Closer); ok {
		return closer
	}
	return nil
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
