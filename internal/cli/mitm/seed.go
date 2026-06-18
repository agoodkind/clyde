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
// asks the daemon to build a wire baseline from the capture store's deduped
// shape corpus and write it as the current baseline for an upstream. The
// daemon normally creates baselines from live captures, so this command is the
// manual escape hatch an operator uses to force a baseline from already-captured
// shapes.
func newBaselineSeedCmd(f *cli.Factory) *cobra.Command {
	var (
		upstream  string
		includeUA []string
		excludeUA []string
	)
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Write a MITM baseline from the capture store's deduped shape corpus",
		Long: "Seed builds a wire baseline from the deduped native-request " +
			"shapes already captured in the MITM capture store and writes it " +
			"as the current baseline for the given upstream. The provider " +
			"filter is derived from the upstream name. Use --include-ua / " +
			"--exclude-ua to scope which captured caller flavor seeds the " +
			"baseline (for example --include-ua claude-cli). The daemon " +
			"performs the build and write against its capture store.",
		Example: "clyde mitm baseline seed --upstream claude-code --include-ua claude-cli",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := daemon.SeedBaseline(cmd.Context(), upstream, includeUA, excludeUA)
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.seed_baseline.failed", "concern", "cli.mitm", "component", "cli", "upstream", upstream, "err", err)
				return fmt.Errorf("baseline seed: %w", err)
			}
			var out strings.Builder
			fmt.Fprintf(&out, "upstream: %s\n", result.Upstream)
			fmt.Fprintf(&out, "flavors: %d\n", result.Flavors)
			return response.WriteText(cmd.Context(), f.IOStreams.Out, out.String())
		},
	}
	cmd.Flags().StringVar(&upstream, "upstream", "", "Upstream name, e.g. claude-code or codex-cli (required)")
	cmd.Flags().StringSliceVar(&includeUA, "include-ua", nil, "Only seed from shapes whose User-Agent contains one of these substrings")
	cmd.Flags().StringSliceVar(&excludeUA, "exclude-ua", nil, "Drop shapes whose User-Agent contains one of these substrings")
	_ = cmd.MarkFlagRequired("upstream")
	return cmd
}
