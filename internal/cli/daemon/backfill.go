package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/response"
)

// newBackfillConversationScalarsCmd builds the one-shot scalar-backfill command.
// It reads every conversation clyde knows from the on-disk index and sends each
// conversation's workspace root and archived status to the lm-semantic-search
// engine, which writes them onto the rows whose workspace_root is empty,
// preserving each row's dense vector so nothing is re-embedded. It is read-only
// by default and writes only when --execute is set.
func newBackfillConversationScalarsCmd(f *cli.Factory) *cobra.Command {
	execute := false
	cmd := &cobra.Command{
		Use:     "backfill-conversation-scalars",
		Short:   "Backfill workspace_root and archived onto existing conversation rows",
		Long:    "Send each conversation's workspace root and archived status to the lm-semantic-search engine, which writes them onto the rows whose workspace_root is empty, preserving each row's dense vector so nothing is re-embedded. Runs as a read-only dry-run by default, counting the would-change and orphan rows without writing; pass --execute to perform the write.",
		Example: "clyde daemon backfill-conversation-scalars\nclyde daemon backfill-conversation-scalars --execute",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackfillConversationScalars(cmd.Context(), f, !execute)
		},
	}
	cmd.Flags().BoolVar(&execute, "execute", false, "Perform the write. Without this flag the command is a read-only dry-run.")
	return cmd
}

func runBackfillConversationScalars(ctx context.Context, f *cli.Factory, dryRun bool) error {
	cfg, err := f.Config()
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill.config_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("load config: %w", err)
	}
	index := daemonsvc.NewConversationIndex()
	if refreshErr := index.Refresh(ctx); refreshErr != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill.refresh_failed", "concern", "cli.daemon", "component", "cli", "err", refreshErr)
		return fmt.Errorf("refresh conversation index: %w", refreshErr)
	}
	records, err := index.List(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill.list_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("list conversations: %w", err)
	}
	entries := buildBackfillEntries(records)
	client, err := semsearch.Dial(ctx, cfg.Conversation.Semantic.SocketPath)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill.dial_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("dial semantic engine: %w", err)
	}
	defer func() { _ = client.Close() }()
	changed, orphan, err := client.BackfillConversationScalars(ctx, cfg.Conversation.Semantic.CollectionID, entries, dryRun)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill.failed", "concern", "cli.daemon", "component", "cli", "dry_run", dryRun, "err", err)
		return fmt.Errorf("backfill conversation scalars: %w", err)
	}
	mode := "Backfilled"
	if dryRun {
		mode = "Dry run counted"
	}
	if writeErr := response.WriteResult(ctx, f.IOStreams.Out, f.IOStreams.Err, fmt.Sprintf(
		"%s conversation scalars from %d conversations: %d rows changed, %d orphan rows.\n",
		mode, len(entries), changed, orphan,
	)); writeErr != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill.write_failed", "concern", "cli.daemon", "component", "cli", "err", writeErr)
		return fmt.Errorf("write backfill result: %w", writeErr)
	}
	return nil
}

// buildBackfillEntries projects each conversation into the enrichment entry the
// engine needs: its id, workspace root, and archived status. Multiple artifacts
// can share one derived conversation id (the same session recorded under two
// project dirs), producing several records with conflicting workspace roots. The
// engine keys enrichment by conversation id and lets the last entry win, so
// emitting every record could overwrite a real workspace with an empty one.
// buildBackfillEntries coalesces to one entry per id, preferring a record with a
// non-empty workspace root and then the most recently updated one, and emits the
// entries in first-seen order so the result is deterministic. See CLYDE-538.
func buildBackfillEntries(records []conversation.Record) []semsearch.BackfillScalarEntry {
	best := make(map[string]conversation.Record, len(records))
	for _, record := range records {
		if existing, seen := best[record.ID]; seen && !preferBackfillRecord(record, existing) {
			continue
		}
		best[record.ID] = record
	}
	entries := make([]semsearch.BackfillScalarEntry, 0, len(best))
	emitted := make(map[string]struct{}, len(best))
	for _, record := range records {
		if _, done := emitted[record.ID]; done {
			continue
		}
		emitted[record.ID] = struct{}{}
		chosen := best[record.ID]
		entries = append(entries, semsearch.BackfillScalarEntry{
			ConversationID: chosen.ID,
			WorkspaceRoot:  chosen.WorkspaceRoot,
			Archived:       chosen.Archived,
		})
	}
	return entries
}

// preferBackfillRecord reports whether candidate is a better backfill enrichment
// source than current for the same conversation id. A record with a non-empty
// workspace root beats one without; among records equal on that, the more
// recently updated one wins. A tie keeps current, so selection is stable.
func preferBackfillRecord(candidate, current conversation.Record) bool {
	candidateHasWorkspace := candidate.WorkspaceRoot != ""
	currentHasWorkspace := current.WorkspaceRoot != ""
	if candidateHasWorkspace != currentHasWorkspace {
		return candidateHasWorkspace
	}
	return candidate.UpdatedAt.After(current.UpdatedAt)
}
