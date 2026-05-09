package hook

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/notify"
	claudediscovery "goodkind.io/clyde/internal/providers/claude/discovery"
	"goodkind.io/clyde/internal/session"
)

func handleStartupOrResume(
	ctx context.Context,
	log *slog.Logger,
	deps sessionStartDeps,
	hookData SessionStartInput,
	store session.Store,
	out io.Writer,
	errOut io.Writer,
) string {
	sessionName := resultSessionName(hookData, store)
	envSessionNameValue := strings.TrimSpace(os.Getenv(envSessionName))

	if sessionName != "" {
		if envSessionNameValue != "" && sessionName == envSessionNameValue && !store.Exists(sessionName) && hookData.SessionID != "" {
			if session.ValidateLegacySlugName(envSessionNameValue) == nil {
				autoAdoptSession(log, deps, store, envSessionNameValue, hookData, errOut)
			}
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

		if hookData.TranscriptPath != "" {
			if err := saveTranscriptPath(store, sessionName, hookData.TranscriptPath); err != nil {
				log.WarnContext(ctx, "hook.sessionstart.transcript_save_failed",
					"component", "hook",
					"subject", "sessionstart",
					"session", sessionName,
					"err", err,
				)
				_, _ = fmt.Fprintf(errOut, "Warning: failed to save transcript path: %v\n", err)
			} else {
				log.InfoContext(ctx, "hook.sessionstart.transcript_saved",
					"component", "hook",
					"subject", "sessionstart",
					"session", sessionName,
					"transcript", hookData.TranscriptPath,
				)
			}
		}
	}

	outputContexts(log, store, sessionName, out)
	return sessionName
}

func autoAdoptSession(
	log *slog.Logger,
	deps sessionStartDeps,
	store session.Store,
	name string,
	hookData SessionStartInput,
	errOut io.Writer,
) {
	sess := session.NewSession(name, hookData.SessionID)
	sess.Metadata.SetProviderTranscriptPath(hookData.TranscriptPath)

	var header session.DiscoveryResult
	var headerOK bool
	if hookData.TranscriptPath != "" {
		header, headerOK = claudediscovery.ReadTranscriptHeader(hookData.TranscriptPath)
	}

	if headerOK && strings.TrimSpace(header.WorkspaceRoot) != "" {
		sess.Metadata.WorkDir = header.WorkspaceRoot
		sess.Metadata.WorkspaceRoot = header.WorkspaceRoot
	} else if wd, err := launchWorkDir(deps); err == nil {
		sess.Metadata.WorkDir = wd
		sess.Metadata.WorkspaceRoot = wd
	} else if root, err := deps.findProjectRoot(); err == nil {
		sess.Metadata.WorkspaceRoot = root
	}

	// Populate DisplayTitle from the provider-owned observed name so the TUI
	// surfaces the upstream user-given chat name. The hook handles the pre-named
	// path (CLYDE_SESSION_NAME set), so Name stays authoritative; DisplayTitle is
	// purely decorative.
	if headerOK {
		if observedName := header.GetName(); observedName != "" {
			sess.Metadata.DisplayTitle = observedName
			log.Debug("hook.sessionstart.display_title_captured",
				"component", "hook",
				"subject", "sessionstart",
				"session", name,
				"display_title", observedName,
			)
		}
		if header.IsForked {
			sess.Metadata.IsForkedSession = true
			parentID := header.ForkParent.Normalized().ID
			if parentID != "" {
				if parentSession, err := findSessionByUUIDSession(store, parentID); err == nil {
					sess.Metadata.ParentSession = parentSession.Name
					sess.Metadata.ParentClydeUUID = parentSession.ClydeUUID()
				}
			}
			log.Debug("hook.sessionstart.fork_lineage_captured",
				"component", "hook",
				"subject", "sessionstart",
				"session", name,
				"parent_session", sess.Metadata.ParentSession,
				"parent_session_id", parentID,
			)
		}
	}

	if err := store.Create(sess); err != nil {
		log.Warn("hook.sessionstart.auto_adopt_failed",
			"component", "hook",
			"subject", "sessionstart",
			"session", name,
			"err", err,
		)
		_, _ = fmt.Fprintf(errOut, "Warning: auto-adopt failed for '%s': %v\n", name, err)
		return
	}

	log.Info("hook.sessionstart.session_adopted",
		"component", "hook",
		"subject", "sessionstart",
		"session", name,
		"session_id", hookData.SessionID,
		"display_title", sess.Metadata.DisplayTitle,
	)
}

func launchWorkDir(deps sessionStartDeps) (string, error) {
	if cwd := strings.TrimSpace(os.Getenv("CLYDE_LAUNCH_CWD")); cwd != "" {
		if abs, err := filepath.Abs(cwd); err == nil {
			return abs, nil
		}
		return cwd, nil
	}
	return deps.getwd()
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
	sess.Metadata.SetProviderTranscriptPath(hookData.TranscriptPath)
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

	sess.Metadata.SetProviderTranscriptPath(transcriptPath)
	sess.UpdateLastAccessed()

	if err := store.Update(sess); err != nil {
		hookLog.Warn("hook.sessionstart.transcript_metadata_update_failed",
			"component", "hook",
			"subject", "sessionstart",
			"session", sessionName,
			"transcript", transcriptPath,
			"err", err,
		)
		return fmt.Errorf("failed to update session metadata: %w", err)
	}

	return nil
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
		logRawEvent:     cfg.LogRawEvent,
		getwd:           cfg.Getwd,
		findProjectRoot: cfg.FindProjectRoot,
	}
	if deps.logRawEvent == nil {
		deps.logRawEvent = defaultLogRawEvent
	}
	if deps.getwd == nil {
		deps.getwd = os.Getwd
	}
	if deps.findProjectRoot == nil {
		deps.findProjectRoot = config.FindProjectRoot
	}
	return deps
}

func defaultLogRawEvent(rawJSON []byte, sessionID string) error {
	return notify.LogEvent(rawJSON, sessionID)
}
