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
	if skipped, err := handleSuppressedSessionStart(ctx, log, eventJSON); skipped || err != nil {
		return Result{SkippedDuplicate: true}, err
	}
	if store == nil {
		log.ErrorContext(ctx, "hook.sessionstart.invalid_store",
			"component", "hook",
			"subject", "sessionstart",
			slog.String("err", "nil store"),
		)
		return Result{}, fmt.Errorf("hook: store is nil")
	}
	deps := defaultDeps(cfg)
	hookData, rawJSON, err := decodeSessionStartInput(ctx, log, eventJSON)
	if err != nil {
		return Result{}, err
	}
	logRawSessionStartEvent(ctx, log, deps, hookData, rawJSON, errOut)

	// Process-tree loop-breaker. If an ancestor process is already a
	// `clyde hook sessionstart`, we are inside the daemon's recursive
	// claude -p chain (or some other unintended fanout) and must
	// exit fast to avoid a per-spawn process explosion.
	if skipped, result := skipDuplicateSessionStart(ctx, log, hookData, store); skipped {
		return result, nil
	}

	res := Result{Source: hookData.Source}
	sessionName, err := dispatchSessionStartSource(ctx, log, hookData, store, out, errOut)
	res.SessionName = sessionName
	if err != nil {
		return res, err
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

func handleSuppressedSessionStart(ctx context.Context, log *slog.Logger, eventJSON io.Reader) (bool, error) {
	// Hard kill switch for nested invocations. The daemon sets this on
	// its internal `claude -p` summarizer call so the spawned claude's
	// SessionStart hook can't recurse into the daemon and fan out a
	// hook chain (see internal/daemon/server.go contextSummary path).
	// Drain stdin first so claude doesn't block on the pipe.
	if os.Getenv("CLYDE_SUPPRESS_HOOKS") == "" {
		return false, nil
	}
	_, err := io.Copy(io.Discard, eventJSON)
	if err != nil {
		log.WarnContext(ctx, "hook.sessionstart.suppressed_drain_failed",
			"component", "hook",
			"subject", "sessionstart",
			"err", err,
		)
		return true, fmt.Errorf("drain suppressed sessionstart input: %w", err)
	}
	log.InfoContext(ctx, "hook.sessionstart.suppressed",
		"component", "hook",
		"subject", "sessionstart",
		"reason", "CLYDE_SUPPRESS_HOOKS",
	)
	return true, nil
}

func decodeSessionStartInput(ctx context.Context, log *slog.Logger, eventJSON io.Reader) (SessionStartInput, []byte, error) {
	raw, err := io.ReadAll(eventJSON)
	if err != nil {
		log.ErrorContext(ctx, "hook.sessionstart.read_failed",
			"component", "hook",
			"subject", "sessionstart",
			"err", err,
		)
		return SessionStartInput{}, nil, fmt.Errorf("failed to read hook input: %w", err)
	}

	var hookData SessionStartInput
	if err := json.Unmarshal(raw, &hookData); err != nil {
		log.ErrorContext(ctx, "hook.sessionstart.parse_failed",
			"component", "hook",
			"subject", "sessionstart",
			"err", err,
		)
		return SessionStartInput{}, nil, fmt.Errorf("failed to parse hook input: %w", err)
	}

	log.InfoContext(ctx, "hook.sessionstart.received",
		"component", "hook",
		"subject", "sessionstart",
		"session_id", hookData.SessionID,
		"source", hookData.Source,
		// pid is this hook process; ppid is the Claude process that spawned it,
		// so a stray SessionStart can be traced back to its launching process.
		"pid", os.Getpid(),
		"ppid", os.Getppid(),
	)
	return hookData, append([]byte(nil), raw...), nil
}

func logRawSessionStartEvent(ctx context.Context, log *slog.Logger, deps sessionStartDeps, hookData SessionStartInput, rawJSON []byte, errOut io.Writer) {
	if err := deps.logRawEvent(rawJSON, hookData.SessionID); err != nil {
		log.WarnContext(ctx, "hook.sessionstart.raw_log_failed",
			"component", "hook",
			"subject", "sessionstart",
			"session_id", hookData.SessionID,
			"err", err,
		)
		_, _ = fmt.Fprintf(errOut, "Warning: failed to log event: %v\n", err)
	}
}

func skipDuplicateSessionStart(ctx context.Context, log *slog.Logger, hookData SessionStartInput, store session.Store) (bool, Result) {
	if hasAncestorHook() {
		log.WarnContext(ctx, "hook.sessionstart.ancestor_loop_detected",
			"component", "hook",
			"subject", "sessionstart",
			"session_id", hookData.SessionID,
			"source", hookData.Source,
		)
		return true, Result{
			SkippedDuplicate: true,
			Source:           hookData.Source,
			SessionName:      resultSessionName(hookData, store),
		}
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
		return true, Result{
			SkippedDuplicate: true,
			Source:           hookData.Source,
			SessionName:      resultSessionName(hookData, store),
		}
	}

	markHookExecuted(marker)
	log.DebugContext(ctx, "hook.sessionstart.marker_written",
		"component", "hook",
		"subject", "sessionstart",
		"marker", marker,
	)
	return false, Result{
		SkippedDuplicate: false,
		Source:           "",
		SessionName:      "",
	}
}

func dispatchSessionStartSource(
	ctx context.Context,
	log *slog.Logger,
	hookData SessionStartInput,
	store session.Store,
	out io.Writer,
	errOut io.Writer,
) (string, error) {
	switch hookData.Source {
	case "startup", "resume":
		return handleStartupOrResume(ctx, log, hookData, store, out, errOut), nil
	case "compact":
		sessionName, err := handleCompact(ctx, log, hookData, store, out, errOut)
		if err != nil {
			log.ErrorContext(ctx, "hook.sessionstart.compact_failed",
				"component", "hook",
				"subject", "sessionstart",
				"err", err,
			)
		}
		return sessionName, err
	case "clear":
		sessionName, err := handleClear(ctx, log, hookData, store, out, errOut)
		if err != nil {
			log.ErrorContext(ctx, "hook.sessionstart.clear_failed",
				"component", "hook",
				"subject", "sessionstart",
				"err", err,
			)
		}
		return sessionName, err
	default:
		return handleStartupOrResume(ctx, log, hookData, store, out, errOut), nil
	}
}
