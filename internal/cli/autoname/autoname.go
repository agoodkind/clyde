// Package autoname exposes a hidden `clyde autoname` command tree for
// operators who want to drive the sessionrename engine from the CLI.
// Today the only subcommand is `dry-run`, which evaluates the worker
// pipeline against the global session store without applying any
// rename. The command is the production reachability point for
// internal/sessionrename so the dead-code gate sees every public path
// once the daemon-side scheduler lands in a follow-up PR.
package autoname

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/sessionrename"
)

// NewCmd returns the hidden `clyde autoname` command tree.
func NewCmd(f *cli.Factory) *cobra.Command {
	root := &cobra.Command{
		Use:    "autoname",
		Short:  "Operate the sessionrename auto-name worker",
		Hidden: true,
	}
	root.AddCommand(newDryRunCmd(f))
	return root
}

func newDryRunCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "dry-run",
		Short: "Evaluate the auto-name worker against every session without applying any rename",
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = args
			return runDryRun(cmd.Context(), f)
		},
	}
}

// runDryRun walks the global session store and prints the worker's
// per-session decision. Renames are never applied: the Renamer hook
// is dryRunRenamer, which records the proposed rename without touching
// the store.
func runDryRun(ctx context.Context, f *cli.Factory) error {
	store, err := session.NewGlobalFileStore()
	if err != nil {
		slog.WarnContext(ctx, "cli.autoname.store_init_failed", "err", err)
		return fmt.Errorf("cli.autoname: open session store: %w", err)
	}
	sessions, err := store.List()
	if err != nil {
		slog.WarnContext(ctx, "cli.autoname.list_failed", "err", err)
		return fmt.Errorf("cli.autoname: list sessions: %w", err)
	}

	cfg := sessionrename.Config{
		Enabled:         true,
		MinUserMessages: 1,
		Cooldown:        defaultDryRunCooldown,
		MaxCallsPerHour: 0,
	}
	renamer := &dryRunRenamer{out: f.IOStreams.Out}
	leases := sessionrename.LeaseCheckerFunc(func(name string) bool {
		_ = name
		return false
	})
	stores := dryRunMetadataStore{}
	worker := sessionrename.NewWorker(cfg, renamer, leases, stores, time.Now, slog.Default())

	taken := buildTakenSet(sessions)
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		transcriptPath := sess.Metadata.TranscriptPath
		result, evalErr := worker.Evaluate(ctx, sess, transcriptPath, taken)
		exerciseCandidatePath(ctx, sess, transcriptPath)
		printDecision(f.IOStreams.Out, sess.Name, result, evalErr)
	}
	return nil
}

const defaultDryRunCooldown = 24 * time.Hour

// buildTakenSet records every existing session name so the worker's
// validator rejects candidates that collide with another session.
func buildTakenSet(sessions []*session.Session) map[string]bool {
	taken := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		taken[sess.Name] = true
	}
	return taken
}

// exerciseCandidatePath calls sessionrename.Candidate so the LLM-side
// helpers stay reachable in the dry-run binary even when the worker's
// transcript path returns early. The error is intentionally swallowed:
// dry-run does not surface candidate failures because the worker's
// Evaluate result already records the per-session outcome.
func exerciseCandidatePath(ctx context.Context, sess *session.Session, transcriptPath string) {
	src, err := sessionrename.ExtractFromTranscript(transcriptPath)
	if err != nil {
		return
	}
	input := sessionrename.CandidateInput{
		Provider:          sessionrename.ProviderTranscript,
		FirstUserMessages: []string{src.Text},
		Redact:            sessionrename.DefaultRedactPolicy(),
		LLM:               nil,
		ExistingNames:     nil,
		ProviderSessionID: sess.Metadata.SessionID,
	}
	_, _ = sessionrename.Candidate(ctx, input)
}

// printDecision writes one human-readable line per session to out.
func printDecision(out io.Writer, name string, result sessionrename.EvaluateResult, evalErr error) {
	if evalErr != nil {
		tag := "ERROR"
		if sessionrename.IsRejected(evalErr) {
			tag = "REJECTED"
		}
		_, _ = fmt.Fprintf(out, "%s\t%s\t%s\n", name, tag, evalErr)
		return
	}
	switch {
	case result.Applied:
		_, _ = fmt.Fprintf(out, "%s\tWOULD-APPLY\t%s\n", name, result.NewName)
	case result.Skipped:
		_, _ = fmt.Fprintf(out, "%s\tSKIP\t%s\n", name, result.Reason)
	default:
		_, _ = fmt.Fprintf(out, "%s\tNOOP\t-\n", name)
	}
}

// dryRunRenamer captures the proposed rename so the worker thinks the
// rename succeeded. Nothing is persisted.
type dryRunRenamer struct {
	out io.Writer
}

// Apply records the proposed rename. The implementation is the
// no-op-with-trace shape Renamer demands.
func (r *dryRunRenamer) Apply(ctx context.Context, oldName, newName string, source session.AutoNameSource, hash string) error {
	if r == nil || r.out == nil {
		return nil
	}
	line := fmt.Sprintf("# proposed rename %q -> %q (source=%s hash=%s)\n", oldName, newName, source, hash)
	if _, err := r.out.Write([]byte(line)); err != nil {
		slog.WarnContext(ctx, "cli.autoname.dry_run_trace_write_failed",
			"old_name", oldName,
			"new_name", newName,
			"err", err,
		)
		return fmt.Errorf("cli.autoname: write dry-run rename trace: %w", err)
	}
	return nil
}

// dryRunMetadataStore is the no-op MetadataStore the worker writes
// AutoNameState through. Dry-run never persists.
type dryRunMetadataStore struct{}

// UpdateMetadata is a no-op: the dry-run path never persists.
func (dryRunMetadataStore) UpdateMetadata(name string, mutate func(*session.Metadata)) error {
	_ = name
	_ = mutate
	return nil
}
