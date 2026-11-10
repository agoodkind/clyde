package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/response"
)

const (
	defaultCopySharedRepairRounds = 10
)

type copySharedOptions struct {
	FromCollectionID string
	ToCollectionID   string
	VerifyOnly       bool
	Resume           bool
	CheckpointsOnly  bool
	RepairRounds     int
}

func newCopySharedIndexCmd(f *cli.Factory) *cobra.Command {
	options := copySharedOptions{FromCollectionID: "", ToCollectionID: "", VerifyOnly: false, Resume: false, CheckpointsOnly: false, RepairRounds: defaultCopySharedRepairRounds}
	cmd := &cobra.Command{
		Use:   "copy-shared",
		Short: "Copy a conversation collection into the shared vector layout and verify it",
		Long: "Copy every row from the single-collection source specified by --from into the shared vector layout specified by --to without " +
			"embedding. The command preserves each row's ID, every column, the content hash, the embedding model, and the exact vector bytes. " +
			"Before copying, it applies the source dense index definition to the target vector collection and enables mmap on both target " +
			"collections. After copying, each repair round reads both collections page by page, rewrites missing or different rows from the " +
			"source, removes target rows that the source lacks, removes vectors without a row reference, and compares the collections row by " +
			"row. The command fails unless every row matches within --repair-rounds rounds. It never changes or deletes the source. It refuses " +
			"to run while conversation.semantic.ingestion_enabled is true because rows added to or deleted from the source during the copy " +
			"appear as differences. With --resume, it copies only source rows with a primary key greater than the largest target primary key. " +
			"Row IDs are content hashes, and the repair rounds copy source rows that sort below that key. After every row matches, the command " +
			"copies the source's saved conversation fingerprint records to the target. On the next pass, the target requests conversations " +
			"without a saved record and conversations with a current fingerprint that differs from the saved value. Use --checkpoints-only " +
			"after an earlier copy-shared run reported every row equal to copy only the fingerprint records without reading Milvus. This option " +
			"refuses to run while ingestion is enabled and cannot be combined with --resume or --verify-only.",
		Example: "clyde conversation index copy-shared --from clyde-conversations --to clyde-conversations-shared\n" +
			"clyde conversation index copy-shared --from clyde-conversations --to clyde-conversations-shared --resume\n" +
			"clyde conversation index copy-shared --from clyde-conversations --to clyde-conversations-shared --verify-only",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCopySharedIndex(cmd.Context(), f, options)
		},
	}
	cmd.Flags().StringVar(&options.FromCollectionID, "from", "", "Collection id of the single-collection source.")
	cmd.Flags().StringVar(&options.ToCollectionID, "to", "", "Collection id of the shared vector target.")
	cmd.Flags().BoolVar(&options.VerifyOnly, "verify-only", false, "Compare and repair without the full copy.")
	cmd.Flags().BoolVar(&options.Resume, "resume", false, "Copy only the source rows after the largest primary key the target stores.")
	cmd.Flags().BoolVar(&options.CheckpointsOnly, "checkpoints-only", false, "Copy only saved conversation fingerprint records without reading Milvus after an earlier copy-shared run reported every row equal.")
	cmd.Flags().IntVar(&options.RepairRounds, "repair-rounds", defaultCopySharedRepairRounds, "Set the maximum number of repair rounds, each of which repairs differences and then compares the collections.")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func runCopySharedIndex(ctx context.Context, f *cli.Factory, options copySharedOptions) error {
	if options.RepairRounds < 1 {
		return errors.New("copy shared conversation collection: --repair-rounds must be at least 1")
	}
	cfg, err := f.Config()
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.copy_shared.config_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("load config: %w", err)
	}
	semanticCfg := cfg.Conversation.Semantic
	if semanticCfg.Backend == config.ConversationSemanticBackendLocal {
		return errors.New("copy shared conversation collection: the local backend has no Milvus collection")
	}
	if semanticCfg.IngestionEnabled {
		return errors.New("copy shared conversation collection: set conversation.semantic.ingestion_enabled = false and run `clyde daemon reload` before the copy")
	}
	if options.Resume && options.VerifyOnly {
		return errors.New("copy shared conversation collection: --resume and --verify-only cannot be combined")
	}
	if options.CheckpointsOnly {
		if options.Resume || options.VerifyOnly {
			return errors.New("copy shared conversation collection: --checkpoints-only cannot be combined with --resume or --verify-only")
		}
		records, copyErr := vectorsearch.CopyConversationCheckpoints(daemonsvc.ConversationCheckpointDir(), options.FromCollectionID, options.ToCollectionID)
		if copyErr != nil {
			return fmt.Errorf("copy shared conversation collection: %w", copyErr)
		}
		return writeCopySharedResult(ctx, f, fmt.Sprintf("Copied %d conversation fingerprint records.\n", records))
	}
	semanticCfg.MilvusLayout = config.ConversationSemanticMilvusLayoutShared
	client, err := daemonsvc.OpenConversationSearchClient(ctx, semanticCfg)
	if err != nil {
		// The open error can include embedding API key settings. The log omits it.
		slog.WarnContext(ctx, "cli.daemon.copy_shared.open_failed", "concern", "cli.daemon", "component", "cli")
		return fmt.Errorf("open conversation semantic client: %w", err)
	}
	defer func() { _ = client.Close(context.WithoutCancel(ctx)) }()
	progress := newCopySharedProgress(f.IOStreams.Err)
	copied := int64(-1)
	if !options.VerifyOnly {
		copied, err = client.CopyToShared(ctx, options.FromCollectionID, options.ToCollectionID, options.Resume, progress.callback(ctx))
		if err != nil {
			return fmt.Errorf("copy shared conversation collection: %w", err)
		}
	}
	report, err := reconcileAndVerifySharedCopy(ctx, client, options, progress)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "cli.daemon.copy_shared.equal", "concern", "cli.daemon", "component", "cli",
		"rows", report.SourceRows, "vectors", report.TargetVectors)
	records, err := client.CopySharedCheckpoints(options.FromCollectionID, options.ToCollectionID)
	if err != nil {
		return fmt.Errorf("copy shared conversation collection: %w", err)
	}
	summary := fmt.Sprintf("Verified %d source rows: the shared copy stores %d rows and %d distinct vectors, and every row matches. Copied %d conversation fingerprint records.\n",
		report.SourceRows, report.TargetRows, report.TargetVectors, records)
	if copied >= 0 {
		summary = fmt.Sprintf("Copied %d rows. ", copied) + summary
	}
	return writeCopySharedResult(ctx, f, summary)
}

func writeCopySharedResult(ctx context.Context, f *cli.Factory, summary string) error {
	if writeErr := response.WriteResult(ctx, f.IOStreams.Out, summary); writeErr != nil {
		slog.ErrorContext(ctx, "cli.daemon.copy_shared.write_failed", "concern", "cli.daemon", "component", "cli", "err", writeErr)
		return fmt.Errorf("write copy result: %w", writeErr)
	}
	return nil
}

// reconcileAndVerifySharedCopy runs ReconcileSharedCopy and then VerifySharedCopy
// in each round. It returns when verification finds every row equal and fails
// if the rounds run out first.
func reconcileAndVerifySharedCopy(ctx context.Context, client *vectorsearch.Client, options copySharedOptions, progress *copySharedProgress) (vectorsearch.SharedCopyReport, error) {
	var report vectorsearch.SharedCopyReport
	for round := 1; round <= options.RepairRounds; round++ {
		progress.setRound(round)
		reconciled, err := client.ReconcileSharedCopy(ctx, options.FromCollectionID, options.ToCollectionID, progress.callback(ctx))
		if err != nil {
			slog.ErrorContext(ctx, "cli.daemon.copy_shared.reconcile_failed", "concern", "cli.daemon", "component", "cli", "round", round, "err", err)
			return report, fmt.Errorf("reconcile shared conversation collection: %w", err)
		}
		slog.DebugContext(ctx, "cli.daemon.copy_shared.reconciled", "concern", "cli.daemon", "component", "cli",
			"round", round,
			"source_rows", reconciled.SourceRows,
			"target_rows", reconciled.TargetRows,
			"missing", reconciled.Missing,
			"mismatched", reconciled.Mismatched,
			"dangling", reconciled.Dangling,
			"extra", reconciled.Extra,
			"unreferenced_vectors", reconciled.UnreferencedVectors,
		)
		report, err = client.VerifySharedCopy(ctx, options.FromCollectionID, options.ToCollectionID, progress.callback(ctx))
		if err != nil {
			slog.ErrorContext(ctx, "cli.daemon.copy_shared.verify_failed", "concern", "cli.daemon", "component", "cli", "round", round, "err", err)
			return report, fmt.Errorf("verify shared conversation collection: %w", err)
		}
		slog.DebugContext(ctx, "cli.daemon.copy_shared.verified", "concern", "cli.daemon", "component", "cli",
			"round", round,
			"source_rows", report.SourceRows,
			"target_rows", report.TargetRows,
			"target_vectors", report.TargetVectors,
			"missing", report.MissingCount,
			"mismatched", report.MismatchedCount,
			"extra", report.ExtraCount,
			"dangling", report.DanglingCount,
			"unreferenced_vectors", report.UnreferencedVectors,
		)
		if report.Equal() {
			return report, nil
		}
	}
	unequalErr := fmt.Errorf("shared conversation collection still differs after %d rounds: %d missing, %d mismatched, %d extra, %d dangling rows, %d unreferenced vectors",
		options.RepairRounds, report.MissingCount, report.MismatchedCount, report.ExtraCount, report.DanglingCount, report.UnreferencedVectors)
	slog.ErrorContext(ctx, "cli.daemon.copy_shared.unequal", "concern", "cli.daemon", "component", "cli", "err", unequalErr)
	return report, unequalErr
}
