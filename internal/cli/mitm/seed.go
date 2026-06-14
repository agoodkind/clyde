package mitm

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/response"
)

// newBaselineSeedCmd builds `clyde mitm baseline seed`, a thin wrapper that
// asks the daemon to extract a v2 wire baseline from an existing capture
// transcript and write it to disk. The daemon normally creates baselines from
// live captures, so this command is the manual escape hatch an operator uses to
// seed a baseline from a transcript they already have.
func newBaselineSeedCmd(f *cli.Factory) *cobra.Command {
	var (
		upstream  string
		from      string
		output    string
		includeUA []string
		excludeUA []string
	)
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Write a v2 MITM baseline from an existing capture transcript",
		Long: "Seed extracts a v2 wire baseline (baseline-reference.toml) " +
			"from a capture transcript JSONL and writes it for the given " +
			"upstream. The provider filter is derived from the upstream name. " +
			"Use --include-ua / --exclude-ua to scope which captured caller " +
			"flavor seeds the baseline (for example --include-ua claude-cli). " +
			"The daemon performs the extraction and write.",
		Example: "clyde mitm baseline seed --upstream claude-code --from capture.jsonl --include-ua claude-cli",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := daemon.SeedBaseline(cmd.Context(), upstream, from, output, includeUA, excludeUA)
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.seed_baseline.failed", "concern", "cli.mitm", "component", "cli", "upstream", upstream, "err", err)
				return fmt.Errorf("baseline seed: %w", err)
			}
			var out strings.Builder
			fmt.Fprintf(&out, "wrote: %s\n", result.Written)
			fmt.Fprintf(&out, "upstream: %s\n", result.Upstream)
			fmt.Fprintf(&out, "flavors: %d\n", result.Flavors)
			return response.WriteText(cmd.Context(), f.IOStreams.Out, out.String())
		},
	}
	cmd.Flags().StringVar(&upstream, "upstream", "", "Upstream name, e.g. claude-code or codex-cli (required)")
	cmd.Flags().StringVar(&from, "from", "", "Path to a capture transcript JSONL to extract from (required)")
	cmd.Flags().StringVar(&output, "output", "", "Baseline file path; defaults to <baseline-root>/<upstream>/baseline-reference.toml")
	cmd.Flags().StringSliceVar(&includeUA, "include-ua", nil, "Only seed from records whose User-Agent contains one of these substrings")
	cmd.Flags().StringSliceVar(&excludeUA, "exclude-ua", nil, "Drop records whose User-Agent contains one of these substrings")
	_ = cmd.MarkFlagRequired("upstream")
	_ = cmd.MarkFlagRequired("from")
	return cmd
}
