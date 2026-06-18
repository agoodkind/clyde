package mitm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/mitm/capture"
)

// SeedBaselineResult reports the outcome of seeding a wire baseline from the
// capture store's deduped shape corpus.
type SeedBaselineResult struct {
	Upstream string
	Flavors  int
}

// SeedBaseline builds a wire baseline for upstream from the capture store's
// deduped shape corpus and writes it as the current baseline through the
// store. It is the manual escape hatch an operator uses to force a baseline
// from already-captured shapes rather than waiting for the daemon-owned
// refresher. The provider filter is derived from the upstream name. There is
// no external file read.
func SeedBaseline(ctx context.Context, store *capture.Store, upstream string, includeUA, excludeUA []string) (SeedBaselineResult, error) {
	empty := SeedBaselineResult{Upstream: "", Flavors: 0}
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return empty, errors.New("baseline seed: upstream is required")
	}
	if store == nil {
		return empty, errors.New("baseline seed: capture store is required")
	}

	startedAt := clock.Now().UTC()
	since := startedAt.Add(-baselineShapeWindow)
	shapes, err := store.ShapesForUpstream(ctx, upstream, since)
	if err != nil {
		slog.WarnContext(ctx, "mitm.seed_baseline.read_shapes_failed", "concern", "providers.mitm.lifecycle", "component", "daemon",
			"upstream", upstream, "err", err)
		return empty, fmt.Errorf("baseline seed: read shapes for upstream %q: %w", upstream, err)
	}
	snap, err := ExtractSnapshotFromShapes(shapes, SnapshotOptions{
		UpstreamName:               upstream,
		UpstreamVersion:            "seed-" + startedAt.Format("20060102T150405"),
		MaxBodyDepth:               0,
		EnumThreshold:              0,
		IncludeUserAgentSubstrings: includeUA,
		ExcludeUserAgentSubstrings: excludeUA,
		RequireBodyKeys:            nil,
		ForbidBodyKeys:             nil,
	})
	if err != nil {
		slog.WarnContext(ctx, "mitm.seed_baseline.extract_failed", "concern", "providers.mitm.lifecycle", "component", "daemon",
			"upstream", upstream, "err", err)
		return empty, fmt.Errorf("baseline seed: build snapshot for upstream %q: %w", upstream, err)
	}

	if err := putBaseline(ctx, store, upstream, snap, DiffReport{Upstream: upstream, MissingFlavors: nil, ExtraFlavors: nil, FlavorReports: nil}, startedAt); err != nil {
		slog.WarnContext(ctx, "mitm.seed_baseline.write_failed", "concern", "providers.mitm.lifecycle", "component", "daemon",
			"upstream", upstream, "err", err)
		return empty, fmt.Errorf("baseline seed: write baseline for upstream %q: %w", upstream, err)
	}

	return SeedBaselineResult{Upstream: snap.Upstream.Name, Flavors: len(snap.Flavors)}, nil
}
