package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/providers/registry"
	"goodkind.io/clyde/internal/session"
)

// globalStore returns the global session store, or panics on error.
// Used by commands that always need the global store and treat an error as fatal.
func globalStore(ctx context.Context) (*session.FileStore, error) {
	store, err := session.NewGlobalFileStore()
	if err != nil {
		slog.WarnContext(ctx, "cmd.session.global_store_failed",
			"component", "cli",
			"err", err,
		)
		return nil, fmt.Errorf("failed to open session store: %w", err)
	}
	return store, nil
}

// printResumeInstructions prints how to resume a session after the interactive
// provider exits.
// Skipped for incognito sessions (they auto-delete).
func printResumeInstructions(ctx context.Context, sess *session.Session) {
	if sess.Metadata.IsIncognito {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout)
	_, _ = fmt.Fprintln(os.Stdout, "Resume this session with:")
	_, _ = fmt.Fprintf(os.Stdout, "  clyde resume %s\n", strconv.Quote(sess.Name))
	runtime, err := registry.ForSession(sess, nil)
	if err != nil {
		cmdDispatchLog.Logger().WarnContext(ctx, "cmd.session.resume_instructions_provider_failed",
			"component", "cli",
			"session", sess.Name,
			"provider", sess.ProviderID(),
			"err", err,
		)
	} else {
		for _, line := range runtime.ResumeInstructions(sess) {
			_, _ = fmt.Fprintf(os.Stdout, "  %s\n", line)
		}
	}
	cmdResumeLog.Logger().InfoContext(ctx, "cmd.session.resume_instructions", "session", sess.Name, "session_id", sess.Metadata.ProviderSessionID())
}

// autoUpdateContext sends a fire-and-forget request to the daemon to generate
// a context summary for the session. Extracts recent messages here (avoiding
// import cycles in the daemon package), then sends them via gRPC. The daemon
// runs the LLM call in the background so the wrapper can exit immediately.
func autoUpdateContext(parentCtx context.Context, _ *session.FileStore, sess *session.Session) {
	if sess.Metadata.IsIncognito {
		return
	}

	runtime, err := registry.ForSession(sess, nil)
	if err != nil {
		return
	}

	// Sample 100 messages so the labeler sees the conversation arc.
	recent := runtime.RecentContextMessages(sess, 100, 300)
	if len(recent) == 0 {
		return
	}
	var messages []string
	for _, msg := range recent {
		role := "User"
		if msg.Role == "assistant" {
			role = "Assistant"
		}
		messages = append(messages, fmt.Sprintf("[%s] %s", role, msg.Text))
	}

	ctx := childCommandContext(parentCtx, "session.context.auto_update")
	client, err := daemon.ConnectOrStart(ctx)
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()

	_ = client.UpdateContext(sess.Name, sess.Metadata.WorkspaceRoot, messages)
}

// resolveSessionForResume finds a session using the store's unified resolution,
// with CLI-specific additions: auto-adopt for UUIDs and TUI picker for
// ambiguous matches. Returns nil session (no error) if nothing found; the
// caller can then fall back to the default provider runtime.
func resolveSessionForResume(cmd *cobra.Command, store *session.FileStore, query string) (*session.Session, error) {
	// Unified 4-tier resolution: exact stored name, provider UUID, display
	// title, then single substring match. Anything more ambiguous than a
	// single match is listed and rejected so the user picks unambiguously
	// themselves.
	// The TUI dashboard exists for interactive multi-match selection;
	// this CLI verb stays scriptable.
	if sess, err := store.Resolve(query); err != nil {
		return nil, err
	} else if sess != nil {
		return sess, nil
	}
	matches, err := store.Search(query)
	if err != nil {
		return nil, err
	}
	if len(matches) <= 1 {
		return nil, nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Multiple sessions match %q:\n", query)
	for _, s := range matches {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s (%s)\n", s.Name, s.Metadata.ProviderSessionID())
	}
	return nil, fmt.Errorf("ambiguous session reference %q; specify the exact title, stored name, or provider session id", query)
}
