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
	RepairRounds     int
}

func newCopySharedIndexCmd(f *cli.Factory) *cobra.Command {
	options := copySharedOptions{FromCollectionID: "", ToCollectionID: "", VerifyOnly: false, Resume: false, RepairRounds: defaultCopySharedRepairRounds}
	cmd := &cobra.Command{
		Use:   "copy-shared",
		Short: "Copy a conversation collection into the shared vector layout and verify it",
		Long: "Copy every row of the single-collection conversation collection named by --from into the shared vector layout named by --to, " +
			"without embedding. The copy keeps each row's ID, every column, the content hash, the embedding model, and the exact vector bytes. " +
			"The command then compares the two collections row by row, repairs the rows that differ, and fails unless every row matches. " +
			"Before the copy, it gives the target vector collection the dense index definition of the source and enables mmap on both target collections. " +
			"It never changes or deletes the source collection. The command refuses to run while conversation.semantic.ingestion_enabled is true, " +
			"because rows that the source gains or loses during the copy appear as differences. --resume copies only the source rows after the " +
			"largest primary key the target already stores.",
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
	cmd.Flags().IntVar(&options.RepairRounds, "repair-rounds", defaultCopySharedRepairRounds, "Maximum number of compare and repair rounds.")
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
	report, err := verifyAndRepairSharedCopy(ctx, client, options, progress)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "cli.daemon.copy_shared.equal", "concern", "cli.daemon", "component", "cli",
		"rows", report.SourceRows, "vectors", report.TargetVectors)
	summary := fmt.Sprintf("Verified %d source rows: the shared copy stores %d rows and %d distinct vectors, and every row matches.\n",
		report.SourceRows, report.TargetRows, report.TargetVectors)
	if copied >= 0 {
		summary = fmt.Sprintf("Copied %d rows. ", copied) + summary
	}
	if writeErr := response.WriteResult(ctx, f.IOStreams.Out, summary); writeErr != nil {
		slog.ErrorContext(ctx, "cli.daemon.copy_shared.write_failed", "concern", "cli.daemon", "component", "cli", "err", writeErr)
		return fmt.Errorf("write copy result: %w", writeErr)
	}
	return nil
}

// verifyAndRepairSharedCopy compares the copy with the source and repairs the
// listed rows until every row matches or the repair rounds run out.
func verifyAndRepairSharedCopy(ctx context.Context, client *vectorsearch.Client, options copySharedOptions, progress *copySharedProgress) (vectorsearch.SharedCopyReport, error) {
	var report vectorsearch.SharedCopyReport
	for round := 1; round <= options.RepairRounds; round++ {
		var err error
		progress.setRound(round)
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
		if err := client.RepairSharedCopy(ctx, options.FromCollectionID, options.ToCollectionID, report); err != nil {
			slog.ErrorContext(ctx, "cli.daemon.copy_shared.repair_failed", "concern", "cli.daemon", "component", "cli", "round", round, "err", err)
			return report, fmt.Errorf("repair shared conversation collection: %w", err)
		}
	}
	unequalErr := fmt.Errorf("shared conversation collection still differs after %d rounds: %d missing, %d mismatched, %d extra, %d dangling rows, %d unreferenced vectors",
		options.RepairRounds, report.MissingCount, report.MismatchedCount, report.ExtraCount, report.DanglingCount, report.UnreferencedVectors)
	slog.ErrorContext(ctx, "cli.daemon.copy_shared.unequal", "concern", "cli.daemon", "component", "cli", "err", unequalErr)
	return report, unequalErr
}
