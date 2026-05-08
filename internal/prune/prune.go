package prune

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/outputstyle"
	"goodkind.io/clyde/internal/providers/registry/artifacts"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/clyde/internal/ui"
)

// Kind selects which prune operation Run executes.
type Kind string

const (
	KindEmpty     Kind = "empty"
	KindEphemeral Kind = "ephemeral"
	KindAutoname  Kind = "autoname"
)

// EmptySettings controls how empty-session pruning classifies abandoned sessions.
type EmptySettings struct {
	MaxLines int
	MinAge   time.Duration
}

// DefaultEmptySettings returns conservative defaults matching the legacy prune-empty command.
func DefaultEmptySettings() EmptySettings {
	return EmptySettings{
		MaxLines: 40,
		MinAge:   24 * time.Hour,
	}
}

// Options configures prune operations.
type Options struct {
	DryRun         bool
	SkipConfirm    bool
	Input          io.Reader
	Empty          EmptySettings
	AutonameMinAge time.Duration
}

// DeleteFailure records a single target that could not be removed.
type DeleteFailure struct {
	Target string
	Err    error
}

// Result summarizes a prune run.
type Result struct {
	Considered int
	Pruned     int
	Failures   []DeleteFailure
}

// Run dispatches to PruneEmpty, PruneEphemeral, or PruneAutoname based on kind.
func Run(
	ctx context.Context,
	kind Kind,
	store session.Store,
	log *slog.Logger,
	out io.Writer,
	opts Options,
) (Result, error) {
	if log == nil {
		log = slog.Default()
	}
	log = slogger.WithConcern(log, slogger.ConcernDaemonWorkersPrune)
	switch kind {
	case KindEmpty:
		return PruneEmpty(ctx, store, log, out, opts)
	case KindEphemeral:
		return PruneEphemeral(ctx, store, log, out, opts)
	case KindAutoname:
		return PruneAutoname(ctx, store, log, out, opts)
	default:
		return Result{}, fmt.Errorf("prune: unknown kind %q", kind)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// shortDuration renders a duration in hours or days for human readability.
func shortDuration(d time.Duration) string {
	hours := int(d.Hours())
	if hours < 48 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dd", hours/24)
}

func projectClydeRootForSession(sess *session.Session) string {
	root := sess.Metadata.WorkspaceRoot
	if root == "" {
		root, _ = config.FindProjectRoot()
	}
	return filepath.Join(root, config.ClydeDir)
}

func deleteTrackedSession(
	ctx context.Context,
	log *slog.Logger,
	out io.Writer,
	sess *session.Session,
	store session.Store,
) error {
	projClydeRoot := projectClydeRootForSession(sess)

	deleted, err := artifacts.Delete(ctx, session.DeleteArtifactsRequest{
		Session:   sess,
		ClydeRoot: projClydeRoot,
	})
	if err != nil {
		_, _ = fmt.Fprintln(out, ui.Warning(fmt.Sprintf("Failed to delete provider artifacts: %v", err)))
		log.ErrorContext(ctx, "prune.delete.provider_artifacts_failed",
			"component", "prune",
			"session", sess.Name,
			"provider", sess.ProviderID(),
			"err", err,
		)
	}

	if ok, derr := daemon.DeleteSessionViaDaemon(ctx, sess.Name); ok {
		_ = derr
	} else if err := store.Delete(sess.Name); err != nil {
		log.ErrorContext(ctx, "prune.delete.folder_failed", "component", "prune", "session", sess.Name, "err", err)
		return fmt.Errorf("failed to delete session: %w", err)
	}

	if sess.Metadata.HasCustomOutputStyle {
		if err := outputstyle.DeleteCustomStyleFile(config.GlobalOutputStyleRoot(), sess.StorageKey()); err != nil {
			_, _ = fmt.Fprintln(out, ui.Warning(fmt.Sprintf("Failed to delete output style file: %v", err)))
			log.ErrorContext(ctx, "prune.delete.style_failed", "component", "prune", "session", sess.Name, "err", err)
		}
	}

	transcriptCount := 0
	agentLogCount := 0
	if deleted != nil {
		transcriptCount = len(deleted.Transcripts)
		agentLogCount = len(deleted.AgentLogs)
	}
	_, _ = fmt.Fprintln(out, ui.Success(fmt.Sprintf("Deleted session '%s'", sess.Name)))
	_, _ = fmt.Fprintf(out, "  Session folder, %d transcript(s), %d agent log(s)\n", transcriptCount, agentLogCount)
	log.InfoContext(ctx, "prune.delete.completed",
		"component", "prune",
		"session", sess.Name,
		"transcripts", transcriptCount,
		"agent_logs", agentLogCount,
	)

	return nil
}
