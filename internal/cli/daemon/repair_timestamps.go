package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/response"
)

const (
	defaultRepairTimestampsMaxRows   = 10_000
	repairTimestampsProgressDirName  = "timestamp-repair"
	repairTimestampsProgressFileExt  = ".json"
	repairTimestampsProgressFileMode = 0o600
	repairTimestampsProgressDirMode  = 0o700
	// repairTimestampsSourceStoreSuffix distinguishes source store progress files.
	repairTimestampsSourceStoreSuffix = "-source-store"
)

type repairTimestampsOptions struct {
	DryRun         bool
	Provider       string
	ConversationID string
	Limit          int
	MaxRows        int
	After          string
	Resume         bool
	ProgressPath   string
	SourceStore    bool
	EvidencePath   string
}

type timestampRepairClient interface {
	RepairConversationTimestamps(context.Context, string, []vectorsearch.TimestampRepairSource, bool) (vectorsearch.TimestampRepairReport, error)
	RepairTimestampsFromSource(context.Context, string, []vectorsearch.TimestampSourceRepairSource, bool) (vectorsearch.TimestampSourceRepairReport, error)
	ZeroTimeConversationIDs(context.Context, string, string) ([]string, error)
}

type timestampRepairProgress struct {
	Collection  string                             `json:"collection"`
	Provider    string                             `json:"provider,omitempty"`
	SourceStore bool                               `json:"source_store,omitempty"`
	Cursor      string                             `json:"cursor"`
	Complete    bool                               `json:"complete"`
	UpdatedAt   time.Time                          `json:"updated_at"`
	Totals      vectorsearch.TimestampRepairReport `json:"totals"`
	Failed      []string                           `json:"failed,omitempty"`
	LastRunRows int                                `json:"last_run_rows_written"`
}

type repairTimestampsRun struct {
	options        repairTimestampsOptions
	collectionID   string
	collectionName string
	contentKinds   conversation.ContentKindSet
	index          conversationDocumentBackfillIndex
	client         timestampRepairClient
	out            io.Writer
	// sourceTotals accumulates source repair counts for the current run.
	sourceTotals *vectorsearch.TimestampSourceRepairReport
	// evidenceOut receives JSON evidence entries when an evidence file is configured.
	evidenceOut io.Writer
}

func newRepairConversationTimestampsCmd(f *cli.Factory) *cobra.Command {
	options := repairTimestampsOptions{DryRun: true, Provider: "", ConversationID: "", Limit: 0, MaxRows: defaultRepairTimestampsMaxRows, After: "", Resume: false, ProgressPath: "", SourceStore: false, EvidencePath: ""}
	execute := false
	cmd := &cobra.Command{
		Use:   "repair-timestamps",
		Short: "Repair timestamps in stored conversation rows",
		Long: "Compare stored conversation rows with the current transcript and repair their timestamps in the single-collection Milvus layout. " +
			"The command matches rows by path and content at the stored split position. It does not embed text. " +
			"A dry run counts changes without writing them. Pass --execute with --conversation or --limit to write changes. " +
			"The command orders conversations by ID and stops before the next conversation after --max-rows changed rows. " +
			"An execute run without --conversation saves progress after each conversation. Use --resume to continue after the saved cursor, or --after to start after a conversation ID.",
		Example: "clyde conversation index repair-timestamps --provider cursor --limit 50\nclyde conversation index repair-timestamps --provider cursor --limit 50 --execute\nclyde conversation index repair-timestamps --provider cursor --limit 50 --resume --execute\nclyde conversation index repair-timestamps --conversation cursor:abc --execute",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.DryRun = !execute
			return runRepairConversationTimestamps(cmd.Context(), f, options)
		},
	}
	cmd.Flags().BoolVar(&execute, "execute", false, "Write timestamp changes instead of running a dry run.")
	cmd.Flags().StringVar(&options.Provider, "provider", "", "Select conversations from this provider.")
	cmd.Flags().StringVar(&options.ConversationID, "conversation", "", "Select one conversation by ID.")
	cmd.Flags().IntVar(&options.Limit, "limit", 0, "Select at most this many conversations.")
	cmd.Flags().IntVar(&options.MaxRows, "max-rows", defaultRepairTimestampsMaxRows, "Stop before the next conversation after this many rows change.")
	cmd.Flags().StringVar(&options.After, "after", "", "Start after this conversation ID.")
	cmd.Flags().BoolVar(&options.Resume, "resume", false, "Continue after the cursor in the progress file.")
	cmd.Flags().StringVar(&options.ProgressPath, "progress-file", "", "Read and write progress at this path.")
	cmd.Flags().BoolVar(&options.SourceStore, "source-store", false, "Match zero timestamp rows against the provider source store.")
	cmd.Flags().StringVar(&options.EvidencePath, "evidence-file", "", "Append source repair evidence as JSON lines to this file.")
	return cmd
}

func runRepairConversationTimestamps(ctx context.Context, f *cli.Factory, options repairTimestampsOptions) error {
	if err := validateRepairTimestampsOptions(options); err != nil {
		return err
	}
	cfg, err := f.Config()
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.config_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("load configuration: %w", err)
	}
	semanticCfg := cfg.Conversation.Semantic
	if semanticCfg.Backend == config.ConversationSemanticBackendLocal {
		return vectorsearch.ErrTimestampRepairUnsupported
	}
	if semanticCfg.MilvusLayout == config.ConversationSemanticMilvusLayoutShared && !options.SourceStore {
		return vectorsearch.ErrTimestampRepairUnsupported
	}
	contentKinds, err := daemonsvc.SemanticContentKinds(semanticCfg)
	if err != nil {
		return fmt.Errorf("resolve content kinds for timestamp repair: %w", err)
	}
	collectionName := vectorsearch.CollectionName(semanticCfg.CollectionID)
	if options.ProgressPath == "" {
		progressName := collectionName
		if options.SourceStore {
			progressName += repairTimestampsSourceStoreSuffix
		}
		options.ProgressPath = filepath.Join(config.DefaultStateDir(), repairTimestampsProgressDirName, progressName+repairTimestampsProgressFileExt)
	}
	client, err := daemonsvc.OpenConversationSearchClient(ctx, semanticCfg)
	if err != nil {
		slog.WarnContext(ctx, "cli.daemon.repair_timestamps.open_failed", "concern", "cli.daemon", "component", "cli")
		return fmt.Errorf("open conversation search client: %w", err)
	}
	defer func() { _ = client.Close(context.WithoutCancel(ctx)) }()
	run := repairTimestampsRun{
		options:        options,
		collectionID:   semanticCfg.CollectionID,
		collectionName: collectionName,
		contentKinds:   contentKinds,
		index:          daemonsvc.NewConversationIndex(),
		client:         client,
		out:            f.IOStreams.Out,
		sourceTotals:   newSourceRepairTotals(),
		evidenceOut:    nil,
	}
	if options.EvidencePath != "" {
		evidenceFile, err := os.OpenFile(options.EvidencePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, repairTimestampsProgressFileMode)
		if err != nil {
			slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.evidence_open_failed", "concern", "cli.daemon", "component", "cli", "path", options.EvidencePath, "err", err)
			return fmt.Errorf("open evidence file %s: %w", options.EvidencePath, err)
		}
		defer func() { _ = evidenceFile.Close() }()
		run.evidenceOut = evidenceFile
	}
	return run.execute(ctx)
}

func validateRepairTimestampsOptions(options repairTimestampsOptions) error {
	conversationID := strings.TrimSpace(options.ConversationID)
	if options.Limit < 0 {
		return errors.New("set --limit to zero or a positive number")
	}
	if options.MaxRows < 1 {
		return errors.New("set --max-rows to at least 1")
	}
	if conversationID != "" && (options.Limit > 0 || options.After != "" || options.Resume || options.Provider != "") {
		return errors.New("use --conversation without --limit, --after, --resume, or --provider")
	}
	if options.After != "" && options.Resume {
		return errors.New("use either --after or --resume")
	}
	if !options.DryRun && conversationID == "" && options.Limit == 0 {
		return errors.New("--execute requires --conversation <id> or --limit <n>")
	}
	return nil
}

func (run repairTimestampsRun) execute(ctx context.Context) error {
	if run.options.Provider != "" && !knownConversationProvider(run.options.Provider) {
		return fmt.Errorf("select a supported provider; %q is unknown", run.options.Provider)
	}
	emptyTotals := vectorsearch.TimestampRepairReport{
		Conversations: 0, RowsRead: 0, RowsCurrent: 0, RowsChanged: 0, RowsWritten: 0,
		RowsUnmatched: 0, RowsMissingTimestamp: 0, RowsAmbiguous: 0, RowsInvalidMetadata: 0,
	}
	progress := timestampRepairProgress{Collection: run.collectionName, Provider: run.options.Provider, SourceStore: run.options.SourceStore, Cursor: "", Complete: false, UpdatedAt: time.Time{}, Totals: emptyTotals, Failed: nil, LastRunRows: 0}
	after := strings.TrimSpace(run.options.After)
	if run.options.Resume {
		stored, err := readTimestampRepairProgress(run.options.ProgressPath)
		if err != nil {
			return err
		}
		if stored.SourceStore != run.options.SourceStore {
			return fmt.Errorf("progress file %s records source-store as %t; the current setting is %t", run.options.ProgressPath, stored.SourceStore, run.options.SourceStore)
		}
		if stored.Collection != run.collectionName || stored.Provider != run.options.Provider {
			return fmt.Errorf("progress file %s records collection %q and provider %q; the current selection uses collection %q and provider %q", run.options.ProgressPath, stored.Collection, stored.Provider, run.collectionName, run.options.Provider)
		}
		progress = stored
		after = stored.Cursor
	}
	if err := run.index.Refresh(ctx); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.refresh_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("refresh the conversation index: %w", err)
	}
	stamped, err := run.index.ListWithStamps(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.list_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("list indexed conversations: %w", err)
	}
	ordered, err := run.repairRecords(ctx, stamped)
	if err != nil {
		return err
	}
	retries := retryRecords(ordered, progress.Failed)
	progress.Failed = nil
	pending := recordsAfter(ordered, after)
	if run.options.Limit > 0 && len(pending) > run.options.Limit {
		pending = pending[:run.options.Limit]
	}
	state := repairLoopState{report: emptyTotals, failed: make([]string, 0), cursor: after}
	if err := run.repairPending(ctx, progress, retries, pending, &state); err != nil {
		return err
	}
	runReport, failed, cursor := state.report, state.failed, state.cursor
	complete := len(recordsAfter(ordered, cursor)) == 0 && len(failed) == 0
	if saveErr := run.saveProgress(progress, cursor, runReport, failed, complete); saveErr != nil {
		return saveErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("timestamp repair stopped after %s: %w", cursor, ctxErr)
	}
	return run.writeResult(ctx, runReport, failed, cursor, complete)
}

// repairLoopState records the current report, failed IDs, and cursor.
type repairLoopState struct {
	report vectorsearch.TimestampRepairReport
	failed []string
	cursor string
}

// repairPending repairs retry records before pending records and saves progress.
func (run repairTimestampsRun) repairPending(ctx context.Context, progress timestampRepairProgress, retries []conversation.Record, pending []conversation.Record, state *repairLoopState) error {
	repair := run.recordRepairer()
	for position, record := range append(slices.Clone(retries), pending...) {
		retry := position < len(retries)
		if state.report.RowsChanged >= run.options.MaxRows || ctx.Err() != nil {
			if retry {
				state.failed = append(state.failed, recordIDs(retries[position:])...)
			}
			return nil
		}
		report, loaded, repairErr := repair(ctx, record)
		state.report.Add(report)
		if repairErr != nil {
			// Keep the cursor before the failed record so a later run can retry it.
			if retry {
				state.failed = append(state.failed, recordIDs(retries[position:])...)
			}
			slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.record_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "cursor", state.cursor, "err", repairErr)
			if saveErr := run.saveProgress(progress, state.cursor, state.report, state.failed, false); saveErr != nil {
				return errors.Join(fmt.Errorf("repair timestamps for %s: %w", record.ID, repairErr), saveErr)
			}
			return fmt.Errorf("repair timestamps for %s: %w", record.ID, repairErr)
		}
		if !loaded {
			state.failed = append(state.failed, record.ID)
		}
		if !retry {
			state.cursor = record.ID
		}
		if saveErr := run.saveProgress(progress, state.cursor, state.report, failedWithRemainingRetries(state.failed, retries, position+1), false); saveErr != nil {
			return saveErr
		}
	}
	return nil
}

// failedWithRemainingRetries returns failed IDs followed by the IDs of retries
// starting at next.
func failedWithRemainingRetries(failed []string, retries []conversation.Record, next int) []string {
	if next >= len(retries) {
		return failed
	}
	return append(slices.Clone(failed), recordIDs(retries[next:])...)
}

// recordRepairer repairs the stored timestamps of one conversation record.
type recordRepairer func(context.Context, conversation.Record) (vectorsearch.TimestampRepairReport, bool, error)

// recordRepairer returns repairSourceStoreRecord when the run uses the source store,
// and repairTranscriptRecord otherwise. repairPending selects the function once
// before its loop over records.
func (run repairTimestampsRun) recordRepairer() recordRepairer {
	if run.options.SourceStore {
		return run.repairSourceStoreRecord
	}
	return run.repairTranscriptRecord
}

// repairTranscriptRecord loads transcript documents and requests timestamp repair.
// If loading fails, it logs the error and returns false without an error.
func (run repairTimestampsRun) repairTranscriptRecord(ctx context.Context, record conversation.Record) (vectorsearch.TimestampRepairReport, bool, error) {
	var empty vectorsearch.TimestampRepairReport
	source, loadErr := run.loadSource(ctx, record)
	if loadErr != nil {
		slog.WarnContext(ctx, "cli.daemon.repair_timestamps.load_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", loadErr)
		return empty, false, nil
	}
	report, repairErr := run.client.RepairConversationTimestamps(ctx, run.collectionID, []vectorsearch.TimestampRepairSource{source}, run.options.DryRun)
	if repairErr != nil {
		return report, true, fmt.Errorf("repair timestamps from transcript: %w", repairErr)
	}
	return report, true, nil
}

// repairSourceStoreRecord loads source store documents and requests timestamp repair.
// It adds the report to source totals and writes evidence when configured.
func (run repairTimestampsRun) repairSourceStoreRecord(ctx context.Context, record conversation.Record) (vectorsearch.TimestampRepairReport, bool, error) {
	var empty vectorsearch.TimestampRepairReport
	source, loadErr := run.loadStoreSource(ctx, record)
	if loadErr != nil {
		slog.WarnContext(ctx, "cli.daemon.repair_timestamps.load_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", loadErr)
		readError := vectorsearch.SourceRepairEvidence{ConversationID: record.ID, Family: "", Rows: nil, Outcome: vectorsearch.SourceOutcomeReadError, Candidates: nil, Chosen: 0, Match: ""}
		return empty, false, errors.Join(fmt.Errorf("load source store: %w", loadErr), run.writeEvidence([]vectorsearch.SourceRepairEvidence{readError}))
	}
	report, repairErr := run.client.RepairTimestampsFromSource(ctx, run.collectionID, []vectorsearch.TimestampSourceRepairSource{source}, run.options.DryRun)
	run.sourceTotals.Add(report)
	evidenceErr := run.writeEvidence(report.Evidence)
	if repairErr != nil {
		return report.TimestampRepairReport, true, errors.Join(fmt.Errorf("repair timestamps from source store: %w", repairErr), evidenceErr)
	}
	if evidenceErr != nil {
		return report.TimestampRepairReport, true, evidenceErr
	}
	return report.TimestampRepairReport, true, nil
}

// writeEvidence writes each evidence entry as JSON when an output is configured.
func (run repairTimestampsRun) writeEvidence(evidence []vectorsearch.SourceRepairEvidence) error {
	if run.evidenceOut == nil {
		return nil
	}
	encoder := json.NewEncoder(run.evidenceOut)
	for _, entry := range evidence {
		if err := encoder.Encode(entry); err != nil {
			slog.Error("cli.daemon.repair_timestamps.evidence_write_failed", "concern", "cli.daemon", "component", "cli", "err", err)
			return fmt.Errorf("write source repair evidence: %w", err)
		}
	}
	return nil
}

// formatOutcomes sorts outcome names and formats their row counts.
func formatOutcomes(outcomes map[string]int) string {
	names := make([]string, 0, len(outcomes))
	for name := range outcomes {
		names = append(names, name)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, outcomes[name]))
	}
	return strings.Join(parts, ", ")
}

// retryRecords selects records with failed IDs in the order of ordered.
func retryRecords(ordered []conversation.Record, failedIDs []string) []conversation.Record {
	retries := make([]conversation.Record, 0, len(failedIDs))
	for _, record := range ordered {
		if slices.Contains(failedIDs, record.ID) {
			retries = append(retries, record)
		}
	}
	return retries
}

func recordIDs(records []conversation.Record) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

func newSourceRepairTotals() *vectorsearch.TimestampSourceRepairReport {
	return &vectorsearch.TimestampSourceRepairReport{
		TimestampRepairReport: vectorsearch.TimestampRepairReport{
			Conversations: 0, RowsRead: 0, RowsCurrent: 0, RowsChanged: 0, RowsWritten: 0,
			RowsUnmatched: 0, RowsMissingTimestamp: 0, RowsAmbiguous: 0, RowsInvalidMetadata: 0,
		},
		RowsNotZeroTime: 0, RowsEmptyContent: 0, RowsIncomplete: 0, Evidence: nil, Outcomes: map[string]int{},
	}
}

// loadStoreSource builds semantic documents from a provider's source store messages.
func (run repairTimestampsRun) loadStoreSource(ctx context.Context, record conversation.Record) (vectorsearch.TimestampSourceRepairSource, error) {
	empty := vectorsearch.TimestampSourceRepairSource{ConversationID: record.ID, Documents: nil, MatchText: nil, CopyDocuments: nil, ToolCalls: nil, ToolCallsComplete: false}
	reader, found := daemonsvc.ConversationSourceStoreReader(record.Provider)
	if !found {
		return empty, fmt.Errorf("source store reader is unavailable for provider %q", record.Provider)
	}
	empty.MatchText = reader.SourceMatchText
	kinds := conversation.NewContentKindSet(conversation.ContentKindChat, conversation.ContentKindThinking, conversation.ContentKindToolCalls)
	source, stored, err := reader.SourceStoreMessages(ctx, record, daemonsvc.SemanticConversationLoadOptions(kinds))
	if err != nil {
		slog.WarnContext(ctx, "cli.daemon.repair_timestamps.source_store_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", err)
		return empty, fmt.Errorf("read source store messages: %w", err)
	}
	messages := source.Messages
	if !stored {
		if strings.TrimSpace(record.ArtifactPath) == "" {
			return empty, nil
		}
		// Load transcript messages when the source store does not contain the conversation.
		messages, err = run.index.LoadMessagesWithOptions(record, daemonsvc.SemanticConversationLoadOptions(kinds))
		if err != nil {
			slog.WarnContext(ctx, "cli.daemon.repair_timestamps.transcript_source_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", err)
			return empty, fmt.Errorf("read transcript source messages: %w", err)
		}
	}
	built, err := daemonsvc.BuildSemanticConversationDocuments(record, messages, kinds)
	if err != nil {
		slog.WarnContext(ctx, "cli.daemon.repair_timestamps.source_documents_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", err)
		return empty, fmt.Errorf("build source store documents: %w", err)
	}
	empty.Documents = built.Docs
	copies, err := daemonsvc.BuildSemanticConversationDocuments(record, source.Copies, kinds)
	if err != nil {
		slog.WarnContext(ctx, "cli.daemon.repair_timestamps.source_copies_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", err)
		return empty, fmt.Errorf("build source copy documents: %w", err)
	}
	empty.CopyDocuments = copies.Docs
	empty.ToolCallsComplete = source.ToolCallsComplete
	empty.ToolCalls = make([]vectorsearch.SourceToolTime, 0, len(source.ToolCalls))
	for _, call := range source.ToolCalls {
		stamp := vectorsearch.MissingTimestampUnix
		if !call.Timestamp.IsZero() {
			stamp = call.Timestamp.Unix()
		}
		empty.ToolCalls = append(empty.ToolCalls, vectorsearch.SourceToolTime{
			Name: call.Tool.Name, Display: call.Tool.Display, LangHint: call.Tool.DisplayLang, TimestampUnix: stamp, Reason: call.Reason, MessageStart: call.MessageStart,
		})
	}
	return empty, nil
}

func (run repairTimestampsRun) loadSource(ctx context.Context, record conversation.Record) (vectorsearch.TimestampRepairSource, error) {
	empty := vectorsearch.TimestampRepairSource{ConversationID: record.ID, Documents: nil, NormalizeStoredText: nil}
	messages, err := run.index.LoadMessagesWithOptions(record, daemonsvc.SemanticConversationLoadOptions(run.contentKinds))
	if err != nil {
		slog.WarnContext(ctx, "cli.daemon.repair_timestamps.messages_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", err)
		return empty, fmt.Errorf("load conversation messages: %w", err)
	}
	built, err := daemonsvc.BuildSemanticConversationDocuments(record, messages, run.contentKinds)
	if err != nil {
		slog.WarnContext(ctx, "cli.daemon.repair_timestamps.documents_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", err)
		return empty, fmt.Errorf("build conversation documents: %w", err)
	}
	slog.DebugContext(ctx, "cli.daemon.repair_timestamps.source_loaded", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "documents", len(built.Docs))
	source := vectorsearch.TimestampRepairSource{ConversationID: record.ID, Documents: built.Docs, NormalizeStoredText: nil}
	if normalizer, found := daemonsvc.ConversationStoredTextNormalizer(record.Provider); found {
		source.NormalizeStoredText = normalizer.NormalizeStoredText
	}
	return source, nil
}

func (run repairTimestampsRun) saveProgress(base timestampRepairProgress, cursor string, runReport vectorsearch.TimestampRepairReport, failed []string, complete bool) error {
	if run.options.DryRun || strings.TrimSpace(run.options.ConversationID) != "" {
		return nil
	}
	next := base
	next.Cursor = cursor
	next.Complete = complete
	next.UpdatedAt = clock.Now().UTC()
	next.Totals.Add(runReport)
	next.Failed = append(slices.Clone(base.Failed), failed...)
	next.LastRunRows = runReport.RowsWritten
	return writeTimestampRepairProgress(run.options.ProgressPath, next)
}

func (run repairTimestampsRun) writeResult(ctx context.Context, report vectorsearch.TimestampRepairReport, failed []string, cursor string, complete bool) error {
	mode := "Examined"
	if run.options.DryRun {
		mode = "Examined in a dry run"
	}
	nextCursor := cursor
	if complete {
		nextCursor = "none"
	}
	text := fmt.Sprintf(
		"%s timestamps in %s for %d conversations: read %d rows, found %d current, %d changed, %d written, %d unmatched, %d without a source timestamp, %d ambiguous, %d with invalid metadata, and %d conversations that failed to load. Next cursor: %s.\n",
		mode, run.collectionName, report.Conversations, report.RowsRead, report.RowsCurrent, report.RowsChanged, report.RowsWritten,
		report.RowsUnmatched, report.RowsMissingTimestamp, report.RowsAmbiguous, report.RowsInvalidMetadata, len(failed), nextCursor,
	)
	if run.options.SourceStore {
		text += fmt.Sprintf("Already dated rows: %d; rows with empty content: %d; rows with split parts that could not be joined: %d.\n", run.sourceTotals.RowsNotZeroTime, run.sourceTotals.RowsEmptyContent, run.sourceTotals.RowsIncomplete)
		text += fmt.Sprintf("Rows by outcome: %s.\n", formatOutcomes(run.sourceTotals.Outcomes))
	}
	if !run.options.DryRun && strings.TrimSpace(run.options.ConversationID) == "" {
		text += fmt.Sprintf("Saved progress in %s.\n", run.options.ProgressPath)
	}
	if err := response.WriteResult(ctx, run.out, text); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.write_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("write timestamp repair result: %w", err)
	}
	return nil
}

// repairRecords selects indexed records or conversations with zero timestamp rows.
func (run repairTimestampsRun) repairRecords(ctx context.Context, stamped []conversation.StampedRecord) ([]conversation.Record, error) {
	if !run.options.SourceStore {
		return repairTimestampRecords(stamped, run.options.Provider, run.options.ConversationID)
	}
	storedIDs, err := run.client.ZeroTimeConversationIDs(ctx, run.collectionID, run.options.Provider)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.stored_ids_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return nil, fmt.Errorf("list conversations with zero timestamp rows: %w", err)
	}
	indexed, err := repairTimestampRecords(stamped, run.options.Provider, "")
	if err != nil {
		return nil, err
	}
	byID := make(map[string]conversation.Record, len(indexed))
	for _, record := range indexed {
		byID[record.ID] = record
	}
	selected := strings.TrimSpace(run.options.ConversationID)
	ordered := make([]conversation.Record, 0, len(storedIDs))
	for _, id := range storedIDs {
		if selected != "" && id != selected {
			continue
		}
		record, found := byID[id]
		if !found {
			stored, ok := storedOnlyRecord(id)
			if !ok {
				slog.WarnContext(ctx, "cli.daemon.repair_timestamps.stored_id_unparsed", "concern", "cli.daemon", "component", "cli", "conversation_id", id)
				continue
			}
			record = stored
		}
		ordered = append(ordered, record)
	}
	if selected != "" && len(ordered) == 0 {
		return nil, fmt.Errorf("cannot find zero timestamp rows for conversation %q", selected)
	}
	return ordered, nil
}

// storedOnlyRecord builds a record from a provider-prefixed conversation ID.
func storedOnlyRecord(id string) (conversation.Record, bool) {
	label, nativeID, cut := strings.Cut(id, ":")
	provider, known := providerid.Parse(label)
	record := conversation.Record{
		ID: id, Provider: provider, NativeID: nativeID, Selector: "", Lineage: nil, Origin: conversation.OriginUnspecified,
		Title: "", TitleUncertain: false, WorkspaceRoot: "", ArtifactPath: "", ArtifactKind: "", Model: "",
		CreatedAt: time.Time{}, UpdatedAt: time.Time{}, SizeBytes: 0, Archived: false, LatestRequestID: "",
	}
	return record, cut && known && strings.TrimSpace(nativeID) != ""
}

func repairTimestampRecords(stamped []conversation.StampedRecord, provider string, conversationID string) ([]conversation.Record, error) {
	trimmedConversationID := strings.TrimSpace(conversationID)
	best := make(map[string]conversation.Record, len(stamped))
	for _, entry := range stamped {
		record := entry.Record
		recordID := strings.TrimSpace(record.ID)
		if recordID == "" {
			continue
		}
		if trimmedConversationID != "" && recordID != trimmedConversationID {
			continue
		}
		if provider != "" && !strings.HasPrefix(recordID, provider+":") {
			continue
		}
		if existing, seen := best[recordID]; seen && !preferBackfillRecord(record, existing) {
			continue
		}
		best[recordID] = record
	}
	if trimmedConversationID != "" && len(best) == 0 {
		return nil, fmt.Errorf("cannot find conversation %q", trimmedConversationID)
	}
	ids := make([]string, 0, len(best))
	for id := range best {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	ordered := make([]conversation.Record, 0, len(ids))
	for _, id := range ids {
		ordered = append(ordered, best[id])
	}
	return ordered, nil
}

func recordsAfter(ordered []conversation.Record, cursor string) []conversation.Record {
	if cursor == "" {
		return ordered
	}
	position, found := slices.BinarySearchFunc(ordered, cursor, func(record conversation.Record, target string) int {
		return strings.Compare(record.ID, target)
	})
	if found {
		position++
	}
	return ordered[position:]
}

func knownConversationProvider(provider string) bool {
	for _, known := range daemonsvc.ConversationProviders() {
		if known.String() == provider {
			return true
		}
	}
	return false
}

func readTimestampRepairProgress(path string) (timestampRepairProgress, error) {
	var progress timestampRepairProgress
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("cli.daemon.repair_timestamps.progress_read_failed", "concern", "cli.daemon", "component", "cli", "path", path, "err", err)
		return progress, fmt.Errorf("read progress file %s to resume: %w", path, err)
	}
	if err := json.Unmarshal(data, &progress); err != nil {
		slog.Warn("cli.daemon.repair_timestamps.progress_decode_failed", "concern", "cli.daemon", "component", "cli", "path", path, "err", err)
		return progress, fmt.Errorf("parse progress file %s: %w", path, err)
	}
	return progress, nil
}

func writeTimestampRepairProgress(path string, progress timestampRepairProgress) (err error) {
	defer func() {
		if err != nil {
			slog.Error("cli.daemon.repair_timestamps.progress_write_failed", "concern", "cli.daemon", "component", "cli", "path", path, "err", err)
			err = fmt.Errorf("save progress file %s: %w", path, err)
			return
		}
		slog.Debug("cli.daemon.repair_timestamps.progress_written", "concern", "cli.daemon", "component", "cli", "path", path, "cursor", progress.Cursor)
	}()
	if err := os.MkdirAll(filepath.Dir(path), repairTimestampsProgressDirMode); err != nil {
		return fmt.Errorf("create the progress directory: %w", err)
	}
	data, err := json.MarshalIndent(progress, "", "  ")
	if err != nil {
		return fmt.Errorf("encode timestamp repair progress: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary progress file: %w", err)
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("write temporary progress file: %w", err)
	}
	if err := temporary.Chmod(repairTimestampsProgressFileMode); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("set temporary progress file permissions: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("sync temporary progress file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close temporary progress file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("replace the progress file: %w", err)
	}
	return nil
}
