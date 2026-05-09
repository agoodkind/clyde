package sessionname

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/slogger"
)

// Rename persists a provider-owned exact session title for the given session.
func Rename(ctx context.Context, sess *session.Session, newName string) error {
	if sess == nil {
		return fmt.Errorf("nil session")
	}
	title := strings.TrimSpace(newName)
	if title == "" {
		return fmt.Errorf("missing session name")
	}
	if err := session.ValidateDisplayName(title); err != nil {
		slog.WarnContext(ctx, "sessionname.invalid_display_name",
			"component", "sessionname",
			"session", sess.Name,
			"err", err,
		)
		return fmt.Errorf("invalid session name: %w", err)
	}
	switch session.NormalizeProviderID(sess.ProviderID()) {
	case session.ProviderUnknown:
		return fmt.Errorf("unsupported session provider %q", sess.ProviderID())
	case session.ProviderClaude:
		return renameClaude(sess, title)
	case session.ProviderCodex:
		return renameCodex(sess, title)
	default:
		return fmt.Errorf("unsupported session provider %q", sess.ProviderID())
	}
}

func renameCodex(sess *session.Session, title string) error {
	sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
	if sessionID == "" {
		return fmt.Errorf("missing codex session id")
	}
	normalized, ok := codexstore.NormalizeThreadName(title)
	if !ok {
		return fmt.Errorf("thread name must not be empty")
	}
	paths, err := codexstore.ResolveStorePathsFromEnv()
	if err != nil {
		slog.Warn("sessionname.codex.paths_failed",
			"component", "codex",
			"subcomponent", "sessionname",
			"concern", slogger.ConcernProviderCodexLifecycle,
			"session", sess.Name,
			"session_id", sessionID,
			"err", err,
		)
		return fmt.Errorf("resolve codex store paths: %w", err)
	}
	if err := codexstore.AppendThreadName(paths, sessionID, normalized); err != nil {
		slog.Warn("sessionname.codex.rename_failed",
			"component", "codex",
			"subcomponent", "sessionname",
			"concern", slogger.ConcernProviderCodexLifecycle,
			"session", sess.Name,
			"session_id", sessionID,
			"err", err,
		)
		return fmt.Errorf("append codex thread name: %w", err)
	}
	return nil
}
