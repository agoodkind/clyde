package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/response"
)

const defaultVerifySample = 300

func newVerifySearchIndexCmd(f *cli.Factory) *cobra.Command {
	sample := defaultVerifySample
	cmd := &cobra.Command{
		Use:     "verify-search-index",
		Short:   "Check that the conversation vector index returns stored rows",
		Long:    "Search the dense index of the Milvus conversation collection with the stored vectors of rows spread over the primary key space. A row passes when the first result scores at least 0.999. The command does not modify the collection and exits with an error when a row fails.",
		Example: "clyde daemon verify-search-index\nclyde daemon verify-search-index --sample 1000",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVerifySearchIndex(cmd.Context(), f, sample)
		},
	}
	cmd.Flags().IntVar(&sample, "sample", defaultVerifySample, "Number of rows to check.")
	return cmd
}

func runVerifySearchIndex(ctx context.Context, f *cli.Factory, sample int) error {
	cfg, err := f.Config()
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.verify_index.config_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("load config: %w", err)
	}
	client, err := daemonsvc.OpenConversationSearchClient(ctx, cfg.Conversation.Semantic)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.verify_index.open_failed", "concern", "cli.daemon", "component", "cli")
		return fmt.Errorf("open conversation semantic client: %w", err)
	}
	defer func() { _ = client.Close(context.WithoutCancel(ctx)) }()
	check, err := client.VerifyIndex(ctx, cfg.Conversation.Semantic.CollectionID, sample)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.verify_index.failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return fmt.Errorf("verify conversation index: %w", err)
	}
	slog.InfoContext(ctx, "cli.daemon.verify_index.completed", "concern", "cli.daemon", "component", "cli",
		"checked", check.Checked,
		"found", check.Found,
	)
	summary := fmt.Sprintf("Checked %d rows: %d returned themselves as the first match.\n", check.Checked, check.Found)
	if len(check.Misses) > 0 {
		summary += "Rows that failed: " + strings.Join(check.Misses, ", ") + "\n"
	}
	if writeErr := response.WriteResult(ctx, f.IOStreams.Out, f.IOStreams.Err, summary); writeErr != nil {
		slog.ErrorContext(ctx, "cli.daemon.verify_index.write_failed", "concern", "cli.daemon", "component", "cli", "err", writeErr)
		return fmt.Errorf("write verify result: %w", writeErr)
	}
	if check.Found < check.Checked {
		return fmt.Errorf("verify conversation index: %d of %d rows did not return themselves", check.Checked-check.Found, check.Checked)
	}
	return nil
}
