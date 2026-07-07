package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/response"
	"goodkind.io/clyde/internal/transcript"
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

type backfillConversationDocumentsOptions struct {
	DryRun         bool
	Limit          int
	ConversationID string
}

type conversationDocumentBackfillIndex interface {
	Refresh(context.Context) error
	ListWithStamps(context.Context) ([]conversation.StampedRecord, error)
	LoadMessagesWithOptions(conversation.Record, conversation.LoadOptions) ([]transcript.Message, error)
}

type conversationDocumentBackfillClient interface {
	SyncConversationManifest(context.Context, string, []semsearch.Fingerprint) ([]string, error)
	UpsertConversationDocuments(context.Context, string, []semsearch.SemDoc, []semsearch.Fingerprint) (string, error)
	Close() error
}

type conversationDocumentBackfillDialer func(context.Context, string) (conversationDocumentBackfillClient, error)

func newBackfillConversationDocumentsCmd(f *cli.Factory) *cobra.Command {
	execute := false
	limit := 0
	conversationID := ""
	cmd := &cobra.Command{
		Use:     "backfill-conversation-documents",
		Short:   "Force selected conversation documents back into semantic search",
		Long:    "Build semantic conversation documents from the current Clyde conversation index and optionally force those selected documents back into lm-semantic-search. Runs as a read-only dry-run by default; pass --execute to upsert documents.",
		Example: "clyde daemon backfill-conversation-documents --limit 10\nclyde daemon backfill-conversation-documents --conversation codex:abc --execute",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackfillConversationDocuments(cmd.Context(), f, backfillConversationDocumentsOptions{
				DryRun:         !execute,
				Limit:          limit,
				ConversationID: conversationID,
			})
		},
	}
	cmd.Flags().BoolVar(&execute, "execute", false, "Perform the document upsert. Without this flag the command is a read-only dry-run.")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum number of conversations to select. Zero selects all conversations.")
	cmd.Flags().StringVar(&conversationID, "conversation", "", "Select one conversation id to backfill.")
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

func runBackfillConversationDocuments(ctx context.Context, f *cli.Factory, options backfillConversationDocumentsOptions) error {
	index := daemonsvc.NewConversationIndex()
	return runBackfillConversationDocumentsWithDeps(ctx, f, options, index, func(dialCtx context.Context, socketPath string) (conversationDocumentBackfillClient, error) {
		return semsearch.Dial(dialCtx, socketPath)
	})
}

func runBackfillConversationDocumentsWithDeps(
	ctx context.Context,
	f *cli.Factory,
	options backfillConversationDocumentsOptions,
	index conversationDocumentBackfillIndex,
	dial conversationDocumentBackfillDialer,
) error {
	if options.Limit < 0 {
		return fmt.Errorf("limit must be non-negative")
	}
	if strings.TrimSpace(options.ConversationID) != "" && options.Limit > 0 {
		return fmt.Errorf("--conversation and --limit cannot be used together")
	}
	if refreshErr := index.Refresh(ctx); refreshErr != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill_documents.refresh_failed", "concern", "cli.daemon", "component", "cli", "err", refreshErr)
		return fmt.Errorf("refresh conversation index: %w", refreshErr)
	}
	stampedRecords, err := index.ListWithStamps(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill_documents.list_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("list conversations with stamps: %w", err)
	}
	selectedRecords, err := selectBackfillDocumentRecords(stampedRecords, options.Limit, options.ConversationID)
	if err != nil {
		return err
	}
	docs, manifest, skipped := buildBackfillConversationDocuments(ctx, index, selectedRecords)
	if options.DryRun {
		return writeBackfillConversationDocumentsResult(ctx, f, "Would send", len(selectedRecords), len(docs), skipped, "", 0)
	}
	if len(selectedRecords) == 0 {
		return writeBackfillConversationDocumentsResult(ctx, f, "Sent", 0, 0, skipped, "", 0)
	}
	cfg, err := f.Config()
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill_documents.config_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("load config: %w", err)
	}
	client, err := dial(ctx, cfg.Conversation.Semantic.SocketPath)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill_documents.dial_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("dial semantic engine: %w", err)
	}
	defer func() { _ = client.Close() }()
	needed, err := client.SyncConversationManifest(ctx, cfg.Conversation.Semantic.CollectionID, manifest)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill_documents.sync_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("sync selected conversation manifest: %w", err)
	}
	jobID, err := client.UpsertConversationDocuments(ctx, cfg.Conversation.Semantic.CollectionID, docs, manifest)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill_documents.upsert_failed", "concern", "cli.daemon", "component", "cli", "documents", len(docs), "err", err)
		return fmt.Errorf("upsert selected conversation documents: %w", err)
	}
	return writeBackfillConversationDocumentsResult(ctx, f, "Sent", len(selectedRecords), len(docs), skipped, jobID, len(needed))
}

func selectBackfillDocumentRecords(stampedRecords []conversation.StampedRecord, limit int, conversationID string) ([]conversation.StampedRecord, error) {
	trimmedConversationID := strings.TrimSpace(conversationID)
	order := make([]string, 0, len(stampedRecords))
	selected := make(map[string]bool, len(stampedRecords))
	best := make(map[string]conversation.StampedRecord, len(stampedRecords))
	for _, stampedRecord := range stampedRecords {
		recordID := strings.TrimSpace(stampedRecord.Record.ID)
		if recordID == "" {
			continue
		}
		if trimmedConversationID != "" && recordID != trimmedConversationID {
			continue
		}
		if !selected[recordID] {
			if trimmedConversationID == "" && limit > 0 && len(order) >= limit {
				continue
			}
			selected[recordID] = true
			order = append(order, recordID)
			best[recordID] = stampedRecord
			continue
		}
		if preferBackfillRecord(stampedRecord.Record, best[recordID].Record) {
			best[recordID] = stampedRecord
		}
	}
	if trimmedConversationID != "" && len(order) == 0 {
		return nil, fmt.Errorf("conversation %q not found", trimmedConversationID)
	}
	selectedRecords := make([]conversation.StampedRecord, 0, len(order))
	for _, recordID := range order {
		selectedRecords = append(selectedRecords, best[recordID])
	}
	return selectedRecords, nil
}

func buildBackfillConversationDocuments(ctx context.Context, index conversationDocumentBackfillIndex, stampedRecords []conversation.StampedRecord) ([]semsearch.SemDoc, []semsearch.Fingerprint, int) {
	docs := make([]semsearch.SemDoc, 0)
	manifest := make([]semsearch.Fingerprint, 0, len(stampedRecords))
	skipped := 0
	for _, stampedRecord := range stampedRecords {
		messages, err := index.LoadMessagesWithOptions(stampedRecord.Record, daemonsvc.SemanticConversationLoadOptions())
		if err != nil {
			slog.WarnContext(ctx, "cli.daemon.backfill_documents.load_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", stampedRecord.Record.ID, "err", err)
			skipped++
			continue
		}
		conversationDocs, err := daemonsvc.BuildSemanticConversationDocuments(stampedRecord.Record, messages)
		if err != nil {
			slog.WarnContext(ctx, "cli.daemon.backfill_documents.build_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", stampedRecord.Record.ID, "err", err)
			skipped++
			continue
		}
		docs = append(docs, conversationDocs...)
		manifest = append(manifest, semsearch.Fingerprint{
			ConversationID: stampedRecord.Record.ID,
			Value:          stampedRecord.Stamp.Fingerprint(),
		})
	}
	return docs, manifest, skipped
}

func writeBackfillConversationDocumentsResult(ctx context.Context, f *cli.Factory, action string, conversations int, documents int, skipped int, jobID string, needed int) error {
	suffix := fmt.Sprintf(", %d skipped %s.", skipped, backfillConversationNoun(skipped))
	if jobID != "" {
		suffix = fmt.Sprintf(", %d skipped %s; manifest sync reported %d needed; job %s.", skipped, backfillConversationNoun(skipped), needed, jobID)
	}
	if writeErr := response.WriteResult(ctx, f.IOStreams.Out, f.IOStreams.Err, fmt.Sprintf(
		"%s conversation documents from %d %s: %d %s%s\n",
		action, conversations, backfillConversationNoun(conversations), documents, backfillDocumentNoun(documents), suffix,
	)); writeErr != nil {
		slog.ErrorContext(ctx, "cli.daemon.backfill_documents.write_failed", "concern", "cli.daemon", "component", "cli", "err", writeErr)
		return fmt.Errorf("write backfill documents result: %w", writeErr)
	}
	return nil
}

func backfillConversationNoun(count int) string {
	if count == 1 {
		return "conversation"
	}
	return "conversations"
}

func backfillDocumentNoun(count int) string {
	if count == 1 {
		return "document"
	}
	return "documents"
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
