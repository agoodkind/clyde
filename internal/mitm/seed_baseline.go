package mitm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
)

// SeedBaselineResult reports the outcome of seeding a v2 wire baseline from a
// capture transcript.
type SeedBaselineResult struct {
	Written  string
	Upstream string
	Flavors  int
}

// SeedBaseline extracts a wire baseline from a capture transcript JSONL and
// writes baseline-reference.toml for the given upstream. The provider filter is
// derived from the upstream name. An explicit output names the file directly;
// otherwise the path is derived from the default baseline root.
func SeedBaseline(ctx context.Context, from, upstream, output string, includeUA, excludeUA []string) (SeedBaselineResult, error) {
	empty := SeedBaselineResult{Written: "", Upstream: "", Flavors: 0}
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return empty, errors.New("baseline seed: upstream is required")
	}
	from = strings.TrimSpace(from)
	if from == "" {
		return empty, errors.New("baseline seed: from is required")
	}

	outputPath := resolveSeedOutputPath(upstream, output)
	outputDir := filepath.Dir(outputPath)

	opts := SnapshotV2Options{
		UpstreamName:               upstream,
		UpstreamVersion:            "",
		ProviderFilter:             ProviderForUpstream(upstream),
		MaxBodyDepth:               0,
		EnumThreshold:              0,
		IncludeUserAgentSubstrings: includeUA,
		ExcludeUserAgentSubstrings: excludeUA,
		RequireBodyKeys:            nil,
		ForbidBodyKeys:             nil,
	}
	snap, err := ExtractSnapshotV2(from, opts)
	if err != nil {
		slog.WarnContext(ctx, "mitm.seed_baseline.extract_failed", "concern", "providers.mitm.lifecycle", "component", "daemon",
			"upstream", upstream, "from", from, "err", err)
		return empty, fmt.Errorf("baseline seed: extract v2 snapshot from %s: %w", from, err)
	}

	written, err := WriteSnapshotV2TOML(snap, outputDir)
	if err != nil {
		slog.WarnContext(ctx, "mitm.seed_baseline.write_failed", "concern", "providers.mitm.lifecycle", "component", "daemon",
			"upstream", upstream, "dir", outputDir, "err", err)
		return empty, fmt.Errorf("baseline seed: write baseline for upstream %q: %w", upstream, err)
	}
	if written != outputPath {
		slog.WarnContext(ctx, "mitm.seed_baseline.path_mismatch", "concern", "providers.mitm.lifecycle", "component", "daemon",
			"upstream", upstream, "written", written, "expected", outputPath)
		return empty, fmt.Errorf("baseline seed: wrote %s but expected %s", written, outputPath)
	}

	return SeedBaselineResult{Written: written, Upstream: snap.Upstream.Name, Flavors: len(snap.Flavors)}, nil
}

// resolveSeedOutputPath returns the baseline-reference.toml path to write. An
// explicit output names the file directly; otherwise the path is derived from
// the default baseline root joined with the upstream's baseline-reference.toml.
func resolveSeedOutputPath(upstream, output string) string {
	if trimmed := strings.TrimSpace(output); trimmed != "" {
		return trimmed
	}
	return BaselineReferencePath(DefaultBaselineRoot(), upstream)
}
