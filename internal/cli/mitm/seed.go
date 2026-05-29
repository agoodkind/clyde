package mitm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/mitm"
)

// seedBaselineCommand builds `clyde mitm seed-baseline`, a thin wrapper
// that extracts a v2 wire baseline from an existing capture transcript
// and writes it to disk. The daemon normally creates baselines from
// live captures, so this command is the manual escape hatch an operator
// uses to seed a baseline from a transcript they already have.
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
			"flavor seeds the baseline (for example --include-ua claude-cli).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSeedBaseline(cmd.Context(), f.IOStreams.Out, upstream, from, output, includeUA, excludeUA)
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

// runSeedBaseline resolves the output path, extracts a v2 snapshot from
// the transcript, and writes reference-v2.toml. WriteSnapshotV2TOML
// writes a fixed reference-v2.toml filename into a directory, so the
// output directory is derived from the resolved output path and the
// produced filename is checked against it.
func runSeedBaseline(ctx context.Context, out io.Writer, upstream, from, output string, includeUA, excludeUA []string) error {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return errors.New("seed-baseline: --upstream is required")
	}
	from = strings.TrimSpace(from)
	if from == "" {
		return errors.New("seed-baseline: --from is required")
	}

	outputPath := resolveSeedOutputPath(upstream, output)
	outputDir := filepath.Dir(outputPath)

	opts := mitm.SnapshotV2Options{
		UpstreamName:               upstream,
		UpstreamVersion:            "",
		ProviderFilter:             mitm.ProviderForUpstream(upstream),
		MaxBodyDepth:               0,
		EnumThreshold:              0,
		IncludeUserAgentSubstrings: includeUA,
		ExcludeUserAgentSubstrings: excludeUA,
		RequireBodyKeys:            nil,
		ForbidBodyKeys:             nil,
	}
	snap, err := mitm.ExtractSnapshotV2(from, opts)
	if err != nil {
		slog.WarnContext(ctx, "cli.mitm.seed_baseline.extract_failed", "concern", "cli.mitm", "upstream", upstream, "from", from, "err", err)
		return fmt.Errorf("seed-baseline: extract v2 snapshot from %s: %w", from, err)
	}

	written, err := mitm.WriteSnapshotV2TOML(snap, outputDir)
	if err != nil {
		slog.WarnContext(ctx, "cli.mitm.seed_baseline.write_failed", "concern", "cli.mitm", "upstream", upstream, "dir", outputDir, "err", err)
		return fmt.Errorf("seed-baseline: write baseline for upstream %q: %w", upstream, err)
	}
	if written != outputPath {
		slog.WarnContext(ctx, "cli.mitm.seed_baseline.path_mismatch", "concern", "cli.mitm", "upstream", upstream, "written", written, "expected", outputPath)
		return fmt.Errorf("seed-baseline: wrote %s but expected %s", written, outputPath)
	}

	fmt.Fprintf(out, "wrote: %s\n", written)
	fmt.Fprintf(out, "upstream: %s\n", snap.Upstream.Name)
	fmt.Fprintf(out, "flavors: %d\n", len(snap.Flavors))
	return nil
}

// resolveSeedOutputPath returns the reference-v2.toml path to write. An
// explicit --output names the file directly; otherwise the path is
// derived from the default baseline root joined with the upstream's
// reference-v2.toml.
func resolveSeedOutputPath(upstream, output string) string {
	if trimmed := strings.TrimSpace(output); trimmed != "" {
		return trimmed
	}
	return mitm.BaselineReferencePath(mitm.DefaultBaselineRoot(), upstream, true)
}
