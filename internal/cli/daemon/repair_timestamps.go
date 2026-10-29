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
	"goodkind.io/clyde/internal/response"
)

const (
	defaultRepairTimestampsMaxRows   = 10_000
	repairTimestampsProgressDirName  = "timestamp-repair"
	repairTimestampsProgressFileExt  = ".json"
	repairTimestampsProgressFileMode = 0o600
	repairTimestampsProgressDirMode  = 0o700
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
}

type timestampRepairClient interface {
	RepairConversationTimestamps(context.Context, string, []vectorsearch.TimestampRepairSource, bool) (vectorsearch.TimestampRepairReport, error)
}

type timestampRepairProgress struct {
	Collection  string                             `json:"collection"`
	Provider    string                             `json:"provider,omitempty"`
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
}

func newRepairConversationTimestampsCmd(f *cli.Factory) *cobra.Command {
	options := repairTimestampsOptions{DryRun: true, Provider: "", ConversationID: "", Limit: 0, MaxRows: defaultRepairTimestampsMaxRows, After: "", Resume: false, ProgressPath: ""}
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
	if semanticCfg.Backend == config.ConversationSemanticBackendLocal || semanticCfg.MilvusLayout == config.ConversationSemanticMilvusLayoutShared {
		return vectorsearch.ErrTimestampRepairUnsupported
	}
	contentKinds, err := daemonsvc.SemanticContentKinds(semanticCfg)
	if err != nil {
		return fmt.Errorf("resolve content kinds for timestamp repair: %w", err)
	}
	collectionName := vectorsearch.CollectionName(semanticCfg.CollectionID)
	if options.ProgressPath == "" {
		options.ProgressPath = filepath.Join(config.DefaultStateDir(), repairTimestampsProgressDirName, collectionName+repairTimestampsProgressFileExt)
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
	progress := timestampRepairProgress{Collection: run.collectionName, Provider: run.options.Provider, Cursor: "", Complete: false, UpdatedAt: time.Time{}, Totals: emptyTotals, Failed: nil, LastRunRows: 0}
	after := strings.TrimSpace(run.options.After)
	if run.options.Resume {
		stored, err := readTimestampRepairProgress(run.options.ProgressPath)
		if err != nil {
			return err
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
	ordered, err := repairTimestampRecords(stamped, run.options.Provider, run.options.ConversationID)
	if err != nil {
		return err
	}
	pending := recordsAfter(ordered, after)
	if run.options.Limit > 0 && len(pending) > run.options.Limit {
		pending = pending[:run.options.Limit]
	}
	runReport := emptyTotals
	failed := make([]string, 0)
	cursor := after
	for _, record := range pending {
		if runReport.RowsChanged >= run.options.MaxRows || ctx.Err() != nil {
			break
		}
		source, loadErr := run.loadSource(ctx, record)
		if loadErr != nil {
			slog.WarnContext(ctx, "cli.daemon.repair_timestamps.load_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", loadErr)
			failed = append(failed, record.ID)
			cursor = record.ID
			continue
		}
		report, repairErr := run.client.RepairConversationTimestamps(ctx, run.collectionID, []vectorsearch.TimestampRepairSource{source}, run.options.DryRun)
		if repairErr != nil {
			if saveErr := run.saveProgress(progress, cursor, runReport, failed, false); saveErr != nil {
				return errors.Join(fmt.Errorf("repair timestamps for %s: %w", record.ID, repairErr), saveErr)
			}
			return fmt.Errorf("repair timestamps for %s: %w", record.ID, repairErr)
		}
		runReport.Add(report)
		cursor = record.ID
		if saveErr := run.saveProgress(progress, cursor, runReport, failed, false); saveErr != nil {
			return saveErr
		}
	}
	complete := len(recordsAfter(ordered, cursor)) == 0
	if saveErr := run.saveProgress(progress, cursor, runReport, failed, complete); saveErr != nil {
		return saveErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("timestamp repair stopped after %s: %w", cursor, ctxErr)
	}
	return run.writeResult(ctx, runReport, failed, cursor, complete)
}

func (run repairTimestampsRun) loadSource(ctx context.Context, record conversation.Record) (vectorsearch.TimestampRepairSource, error) {
	empty := vectorsearch.TimestampRepairSource{ConversationID: record.ID, Documents: nil}
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
	return vectorsearch.TimestampRepairSource{ConversationID: record.ID, Documents: built.Docs}, nil
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
	if !run.options.DryRun && strings.TrimSpace(run.options.ConversationID) == "" {
		text += fmt.Sprintf("Saved progress in %s.\n", run.options.ProgressPath)
	}
	if err := response.WriteResult(ctx, run.out, text); err != nil {
		slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.write_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("write timestamp repair result: %w", err)
	}
	return nil
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
