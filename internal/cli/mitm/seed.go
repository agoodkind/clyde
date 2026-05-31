package mitm

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/daemon"
)

// seedBaselineCommand builds `clyde mitm seed-baseline`, a thin wrapper that
// asks the daemon to extract a v2 wire baseline from an existing capture
// transcript and write it to disk. The daemon normally creates baselines from
// live captures, so this command is the manual escape hatch an operator uses to
// seed a baseline from a transcript they already have.
func seedBaselineCommand(f *cli.Factory) *cobra.Command {
	var (
		upstream  string
		from      string
		output    string
		includeUA []string
		excludeUA []string
	)
	cmd := &cobra.Command{
		Use:   "seed-baseline",
		Short: "Write a v2 MITM baseline from an existing capture transcript",
		Long: "Seed-baseline extracts a v2 wire baseline (reference-v2.toml) " +
			"from a capture transcript JSONL and writes it for the given " +
			"upstream. The provider filter is derived from the upstream name. " +
			"Use --include-ua / --exclude-ua to scope which captured caller " +
			"flavor seeds the baseline (for example --include-ua claude-cli). " +
			"The daemon performs the extraction and write.",
		Example: "clyde mitm seed-baseline --upstream claude-code --from capture.jsonl --include-ua claude-cli",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := daemon.SeedBaseline(cmd.Context(), upstream, from, output, includeUA, excludeUA)
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.seed_baseline.failed", "concern", "cli.mitm", "component", "cli", "upstream", upstream, "err", err)
				return fmt.Errorf("seed-baseline: %w", err)
			}
			out := f.IOStreams.Out
			fmt.Fprintf(out, "wrote: %s\n", result.Written)
			fmt.Fprintf(out, "upstream: %s\n", result.Upstream)
			fmt.Fprintf(out, "flavors: %d\n", result.Flavors)
			return nil
		},
	}
	cmd.Flags().StringVar(&upstream, "upstream", "", "Upstream name, e.g. claude-code or codex-cli (required)")
	cmd.Flags().StringVar(&from, "from", "", "Path to a capture transcript JSONL to extract from (required)")
	cmd.Flags().StringVar(&output, "output", "", "Baseline file path; defaults to <baseline-root>/<upstream>/reference-v2.toml")
	cmd.Flags().StringSliceVar(&includeUA, "include-ua", nil, "Only seed from records whose User-Agent contains one of these substrings")
	cmd.Flags().StringSliceVar(&excludeUA, "exclude-ua", nil, "Drop records whose User-Agent contains one of these substrings")
	_ = cmd.MarkFlagRequired("upstream")
	_ = cmd.MarkFlagRequired("from")
	return cmd
}
