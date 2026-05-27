package hook

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"goodkind.io/clyde/internal/notify"
	"goodkind.io/clyde/internal/session"
)

func handleStartupOrResume(
	ctx context.Context,
	log *slog.Logger,
	hookData SessionStartInput,
	store session.Store,
	out io.Writer,
	errOut io.Writer,
) string {
	// Identity is the provider session UUID. The session record is created by the
	// daemon before launch, so the hook resolves it by UUID here. A Claude that
	// inherited a stale CLYDE_SESSION_NAME no longer adopts or attaches by name.
	sessionName := resultSessionName(hookData, store)
	if sessionName == "" {
		outputContexts(log, store, sessionName, out)
		return ""
	}

	if err := writeSessionIdentityToEnv(sessionName, hookData.SessionID); err != nil {
		log.WarnContext(ctx, "hook.sessionstart.env_write_failed",
			"component", "hook",
			"subject", "sessionstart",
			"key", envLegacySessionName,
			"err", err,
		)
		_, _ = fmt.Fprintf(errOut, "Warning: failed to write session identity to env: %v\n", err)
	}

	persistTranscriptPath(ctx, log, store, sessionName, hookData, errOut)

	outputContexts(log, store, sessionName, out)
	return sessionName
}

// persistTranscriptPath saves the SessionStart transcript path onto the resolved
// session, but only when the incoming provider UUID matches that record. A
// mismatch means an unrelated Claude resolved here, so the write is refused.
func persistTranscriptPath(ctx context.Context, log *slog.Logger, store session.Store, sessionName string, hookData SessionStartInput, errOut io.Writer) {
	if hookData.TranscriptPath == "" {
		return
	}
	if !transcriptWriteAllowed(store, sessionName, hookData.SessionID) {
		log.WarnContext(ctx, "hook.sessionstart.transcript_save_skipped_uuid_mismatch",
			"component", "hook",
			"subject", "sessionstart",
			"session", sessionName,
			"session_id", hookData.SessionID,
			"transcript", hookData.TranscriptPath,
		)
		return
	}
	if err := saveTranscriptPath(store, sessionName, hookData.TranscriptPath); err != nil {
		log.WarnContext(ctx, "hook.sessionstart.transcript_save_failed",
			"component", "hook",
			"subject", "sessionstart",
			"session", sessionName,
			"err", err,
		)
		_, _ = fmt.Fprintf(errOut, "Warning: failed to save transcript path: %v\n", err)
		return
	}
	log.InfoContext(ctx, "hook.sessionstart.transcript_saved",
		"component", "hook",
		"subject", "sessionstart",
		"session", sessionName,
		"transcript", hookData.TranscriptPath,
	)
}

// transcriptWriteAllowed reports whether a SessionStart may persist its
// transcript path onto the resolved session. The write is allowed only when the
// incoming provider session UUID matches the resolved record's current provider
// id, so a Claude process that resolved to this session by an inherited
// CLYDE_SESSION_NAME (rather than by its own UUID) cannot overwrite the path.
func transcriptWriteAllowed(store session.Store, sessionName, incomingSessionID string) bool {
	incomingSessionID = strings.TrimSpace(incomingSessionID)
	if incomingSessionID == "" {
		return false
	}
	sess, err := store.Get(sessionName)
	if err != nil || sess == nil {
		return false
	}
	return sess.Metadata.ProviderSessionID() == incomingSessionID
}

func handleCompact(
	ctx context.Context,
	log *slog.Logger,
	hookData SessionStartInput,
	store session.Store,
	out io.Writer,
	errOut io.Writer,
) (string, error) {
	sessionName, err := resolveSessionName(hookData, store, true)
	if err != nil {
		log.WarnContext(ctx, "hook.sessionstart.resolve_name_failed",
			"component", "hook",
			"subject", "sessionstart",
			"reason", "compact",
			"err", err,
		)
		_, _ = fmt.Fprintf(errOut, "Warning: unable to resolve session name for compact: %v\n", err)
		return "", nil
	}

	if sessionName == "" {
		return "", nil
	}

	sess, err := store.Get(sessionName)
	if err != nil {
		log.WarnContext(ctx, "hook.sessionstart.session_not_found",
			"component", "hook",
			"subject", "sessionstart",
			"reason", "compact",
			"session", sessionName,
			"err", err,
		)
		_, _ = fmt.Fprintf(errOut, "Warning: session '%s' not found: %v\n", sessionName, err)
		return "", nil
	}

	sess.AddPreviousSessionID(hookData.SessionID)
	canonicalPath := canonicalTranscriptPath(realHomeDir(), hookData.TranscriptPath, sess.Metadata.WorkspaceRoot, hookData.SessionID)
	if canonicalPath != "" {
		sess.Metadata.SetProviderTranscriptPath(canonicalPath)
	}
	if canonicalPath != "" && canonicalPath != hookData.TranscriptPath {
		log.InfoContext(ctx, "hook.sessionstart.transcript_canonicalized",
			"component", "hook",
			"subject", "sessionstart",
			"reason", "compact",
			"session", sessionName,
			"reported", hookData.TranscriptPath,
			"canonical", canonicalPath,
		)
	}
	sess.UpdateLastAccessed()

	if err := store.Update(sess); err != nil {
		log.WarnContext(ctx, "hook.sessionstart.metadata_update_failed",
			"component", "hook",
			"subject", "sessionstart",
			"session", sessionName,
			"err", err,
		)
		_, _ = fmt.Fprintf(errOut, "Warning: failed to update session metadata: %v\n", err)
	}

	if err := writeSessionIdentityToEnv(sessionName, hookData.SessionID); err != nil {
		log.WarnContext(ctx, "hook.sessionstart.env_write_failed",
			"component", "hook",
			"subject", "sessionstart",
			"key", envLegacySessionName,
			"session", sessionName,
			"err", err,
		)
		_, _ = fmt.Fprintf(errOut, "Warning: failed to write session identity to env: %v\n", err)
	}

	log.InfoContext(ctx, "hook.sessionstart.compact_handled",
		"component", "hook",
		"subject", "sessionstart",
		"session", sessionName,
		"session_id", hookData.SessionID,
	)

	outputContexts(log, store, sessionName, out)
	return sessionName, nil
}

func handleClear(
	ctx context.Context,
	log *slog.Logger,
	hookData SessionStartInput,
	store session.Store,
	out io.Writer,
	errOut io.Writer,
) (string, error) {
	return handleCompact(ctx, log, hookData, store, out, errOut)
}

func saveTranscriptPath(store session.Store, sessionName, transcriptPath string) error {
	sess, err := store.Get(sessionName)
	if err != nil {
		hookLog.Warn("hook.sessionstart.transcript_session_lookup_failed",
			"component", "hook",
			"subject", "sessionstart",
			"session", sessionName,
			"transcript", transcriptPath,
			"err", err,
		)
		return fmt.Errorf("session '%s' not found: %w", sessionName, err)
	}

	canonicalPath := canonicalTranscriptPath(realHomeDir(), transcriptPath, sess.Metadata.WorkspaceRoot, sess.Metadata.ProviderSessionID())
	if canonicalPath == "" {
		return nil
	}
	if canonicalPath != transcriptPath {
		hookLog.Info("hook.sessionstart.transcript_canonicalized",
			"component", "hook",
			"subject", "sessionstart",
			"session", sessionName,
			"reported", transcriptPath,
			"canonical", canonicalPath,
		)
	}
	sess.Metadata.SetProviderTranscriptPath(canonicalPath)
	sess.UpdateLastAccessed()

	if err := store.Update(sess); err != nil {
		hookLog.Warn("hook.sessionstart.transcript_metadata_update_failed",
			"component", "hook",
			"subject", "sessionstart",
			"session", sessionName,
			"transcript", canonicalPath,
			"err", err,
		)
		return fmt.Errorf("failed to update session metadata: %w", err)
	}

	return nil
}

// realHomeDir returns the user's real home directory for canonicalizing
// transcript paths. The hook process inherits CLAUDE_CONFIG_DIR from claude
// but $HOME stays the operator's home, so [os.UserHomeDir] returns the right
// value here.
func realHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		hookLog.Warn("hook.sessionstart.home_dir_failed",
			"component", "hook",
			"subject", "sessionstart",
			"err", err,
		)
		return ""
	}
	return home
}

func outputContexts(log *slog.Logger, store session.Store, sessionName string, out io.Writer) {
	if sessionName != "" {
		log.Info("hook.sessionstart.operator_session_line",
			"component", "hook",
			"subject", "sessionstart",
			"session", sessionName,
		)
		_, _ = fmt.Fprintf(out, "\nSession name: %s\n", sessionName)
	}

	if sessionName != "" {
		sess, err := store.Get(sessionName)
		if err == nil && sess.Metadata.Context != "" {
			log.Info("hook.sessionstart.operator_context_line",
				"component", "hook",
				"subject", "sessionstart",
				"session", sessionName,
			)
			_, _ = fmt.Fprintf(out, "Context: %s\n", sess.Metadata.Context)
		}
	}
}

func defaultDeps(cfg SessionStartConfig) sessionStartDeps {
	deps := sessionStartDeps{
		logRawEvent: cfg.LogRawEvent,
	}
	if deps.logRawEvent == nil {
		deps.logRawEvent = defaultLogRawEvent
	}
	return deps
}

func defaultLogRawEvent(rawJSON []byte, sessionID string) error {
	return notify.LogEvent(rawJSON, sessionID)
}
