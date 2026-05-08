package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"goodkind.io/clyde/internal/session"
)

// ProcessSessionStart runs the SessionStart hook pipeline: parse JSON, dedupe,
// optional raw event logging, source dispatch, and operator output on out/errOut.
func ProcessSessionStart(
	ctx context.Context,
	store session.Store,
	cfg SessionStartConfig,
	log *slog.Logger,
	eventJSON io.Reader,
	out io.Writer,
	errOut io.Writer,
) (Result, error) {
	if log == nil {
		log = slog.Default()
	}
	// Hard kill switch for nested invocations. The daemon sets this on
	// its internal `claude -p` summarizer call so the spawned claude's
	// SessionStart hook can't recurse into the daemon and fan out a
	// hook chain (see internal/daemon/server.go contextSummary path).
	// Drain stdin first so claude doesn't block on the pipe.
	if os.Getenv("CLYDE_SUPPRESS_HOOKS") != "" {
		_, _ = io.Copy(io.Discard, eventJSON)
		log.InfoContext(ctx, "hook.sessionstart.suppressed",
			"component", "hook",
			"subject", "sessionstart",
			"reason", "CLYDE_SUPPRESS_HOOKS",
		)
		return Result{SkippedDuplicate: true}, nil
	}
	if store == nil {
		log.ErrorContext(ctx, "hook.sessionstart.invalid_store",
			"component", "hook",
			"subject", "sessionstart",
			slog.String("err", "nil store"),
		)
		return Result{}, fmt.Errorf("hook: store is nil")
	}

	raw, err := io.ReadAll(eventJSON)
	if err != nil {
		log.ErrorContext(ctx, "hook.sessionstart.read_failed",
			"component", "hook",
			"subject", "sessionstart",
			"err", err,
		)
		return Result{}, fmt.Errorf("failed to read hook input: %w", err)
	}

	var hookData SessionStartInput
	if err := json.Unmarshal(raw, &hookData); err != nil {
		log.ErrorContext(ctx, "hook.sessionstart.parse_failed",
			"component", "hook",
			"subject", "sessionstart",
			"err", err,
		)
		return Result{}, fmt.Errorf("failed to parse hook input: %w", err)
	}

	log.InfoContext(ctx, "hook.sessionstart.received",
		"component", "hook",
		"subject", "sessionstart",
		"session_id", hookData.SessionID,
		"source", hookData.Source,
	)

	deps := defaultDeps(cfg)
	if err := deps.logRawEvent(raw, hookData.SessionID); err != nil {
		log.WarnContext(ctx, "hook.sessionstart.raw_log_failed",
			"component", "hook",
			"subject", "sessionstart",
			"session_id", hookData.SessionID,
			"err", err,
		)
		_, _ = fmt.Fprintf(errOut, "Warning: failed to log event: %v\n", err)
	}

	// Process-tree loop-breaker. If an ancestor process is already a
	// `clyde hook sessionstart`, we are inside the daemon's recursive
	// claude -p chain (or some other unintended fanout) and must
	// exit fast to avoid a per-spawn process explosion.
	if hasAncestorHook() {
		log.WarnContext(ctx, "hook.sessionstart.ancestor_loop_detected",
			"component", "hook",
			"subject", "sessionstart",
			"session_id", hookData.SessionID,
			"source", hookData.Source,
		)
		return Result{
			SkippedDuplicate: true,
			Source:           hookData.Source,
			SessionName:      resultSessionName(hookData, store),
		}, nil
	}

	marker := hookData.SessionID + ":" + hookData.Source
	if isHookExecuted(marker) {
		log.InfoContext(ctx, "hook.sessionstart.skipped_duplicate",
			"component", "hook",
			"subject", "sessionstart",
			"session_id", hookData.SessionID,
			"source", hookData.Source,
			"marker", marker,
		)
		return Result{
			SkippedDuplicate: true,
			Source:           hookData.Source,
			SessionName:      resultSessionName(hookData, store),
		}, nil
	}

	markHookExecuted(marker)
	log.DebugContext(ctx, "hook.sessionstart.marker_written",
		"component", "hook",
		"subject", "sessionstart",
		"marker", marker,
	)

	res := Result{Source: hookData.Source}

	switch hookData.Source {
	case "startup", "resume":
		res.SessionName = handleStartupOrResume(ctx, log, deps, hookData, store, out, errOut)
	case "compact":
		sessionName, err := handleCompact(ctx, log, hookData, store, out, errOut)
		res.SessionName = sessionName
		if err != nil {
			log.ErrorContext(ctx, "hook.sessionstart.compact_failed",
				"component", "hook",
				"subject", "sessionstart",
				"err", err,
			)
			return res, err
		}
	case "clear":
		sessionName, err := handleClear(ctx, log, hookData, store, out, errOut)
		res.SessionName = sessionName
		if err != nil {
			log.ErrorContext(ctx, "hook.sessionstart.clear_failed",
				"component", "hook",
				"subject", "sessionstart",
				"err", err,
			)
			return res, err
		}
	default:
		res.SessionName = handleStartupOrResume(ctx, log, deps, hookData, store, out, errOut)
	}

	log.InfoContext(ctx, "hook.sessionstart.completed",
		"component", "hook",
		"subject", "sessionstart",
		"session_id", hookData.SessionID,
		"source", hookData.Source,
		"session", res.SessionName,
	)

	return res, nil
}
