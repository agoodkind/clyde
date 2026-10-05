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
		Use:     "verify",
		Short:   "Check dense-index similarity with stored-vector samples",
		Long:    "Check dense-index similarity with stored-vector samples. A sample passes when the first result's cosine score is at least 0.999. The command does not write collection data and reports failing samples.",
		Example: "clyde conversation index verify\nclyde conversation index verify --sample 1000",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVerifySearchIndex(cmd.Context(), f, sample)
		},
	}
	cmd.Flags().IntVar(&sample, "sample", defaultVerifySample, "Number of stored-vector samples to check.")
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
		// The open error can include embedding API key settings. The log omits it.
		slog.WarnContext(ctx, "cli.daemon.verify_index.open_failed", "concern", "cli.daemon", "component", "cli")
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
	summary := fmt.Sprintf("Checked %d samples: %d met the first-result score threshold.\n", check.Checked, check.Found)
	if len(check.Misses) > 0 {
		summary += "Source primary keys of failing samples: " + strings.Join(check.Misses, ", ") + "\n"
	}
	if writeErr := response.WriteResult(ctx, f.IOStreams.Out, f.IOStreams.Err, summary); writeErr != nil {
		slog.ErrorContext(ctx, "cli.daemon.verify_index.write_failed", "concern", "cli.daemon", "component", "cli", "err", writeErr)
		return fmt.Errorf("write verify result: %w", writeErr)
	}
	if check.Found < check.Checked {
		return fmt.Errorf("verify conversation index: %d of %d samples did not meet the first-result score threshold", check.Checked-check.Found, check.Checked)
	}
	return nil
}
