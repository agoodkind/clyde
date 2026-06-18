package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/mitm/capture"
)

// DriftCheckOptions configures one compare-only drift run against the capture
// store's deduped shape corpus. The keep filters scope which captured caller
// flavor the candidate snapshot is built from.
type DriftCheckOptions struct {
	Upstream        string
	IncludeUA       []string
	ExcludeUA       []string
	RequireBodyKeys []string
	ForbidBodyKeys  []string
	Log             *slog.Logger
}

// RunDriftCheck builds a candidate snapshot for one upstream from the store's
// deduped shape corpus, diffs it against the current stored baseline, and
// records the outcome in the drift_checks table. It never writes a baseline;
// that is the refresher's job. Returns the outcome and a non-nil error on
// infrastructure failure. The outcome's Diverged field reports whether the
// candidate disagreed with the stored baseline.
func RunDriftCheck(ctx context.Context, store *capture.Store, opts DriftCheckOptions) (DriftOutcome, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	upstream := strings.TrimSpace(opts.Upstream)
	if upstream == "" {
		return DriftOutcome{}, fmt.Errorf("drift check: upstream is required")
	}
	if store == nil {
		return DriftOutcome{}, fmt.Errorf("drift check: capture store is required")
	}

	startedAt := clock.Now().UTC()
	outcome := DriftOutcome{
		Upstream:  upstream,
		StartedAt: startedAt,
		Timestamp: startedAt,
		Diverged:  false,
		Summary:   "",
		Report:    nil,
	}

	since := startedAt.Add(-baselineShapeWindow)
	shapes, err := store.ShapesForUpstream(ctx, upstream, since)
	if err != nil {
		return outcome, fmt.Errorf("drift check: read shapes: %w", err)
	}
	versionTag := "live-" + startedAt.Format("20060102T150405")
	candidate, err := ExtractSnapshotFromShapes(shapes, SnapshotOptions{
		UpstreamName:               upstream,
		UpstreamVersion:            versionTag,
		IncludeUserAgentSubstrings: opts.IncludeUA,
		ExcludeUserAgentSubstrings: opts.ExcludeUA,
		RequireBodyKeys:            opts.RequireBodyKeys,
		ForbidBodyKeys:             opts.ForbidBodyKeys, MaxBodyDepth: 0, EnumThreshold: 0,
	})
	if err != nil {
		log.WarnContext(ctx, "mitm.drift.no_usable_shapes", "concern", "providers.mitm.wire", "upstream", upstream, "err", err)
		store.RecordCheck(capture.DriftCheck{Timestamp: startedAt, Upstream: upstream, Diverged: false, Summary: "no usable shapes"})
		return outcome, nil
	}

	existingRaw, hasExisting, err := store.CurrentBaseline(ctx, upstream)
	if err != nil {
		return outcome, fmt.Errorf("drift check: read current baseline: %w", err)
	}
	if !hasExisting {
		outcome.Summary = "no baseline for upstream=" + upstream
		store.RecordCheck(capture.DriftCheck{Timestamp: startedAt, Upstream: upstream, Diverged: false, Summary: outcome.Summary})
		return outcome, nil
	}
	existing, err := unmarshalSnapshot(existingRaw)
	if err != nil {
		return outcome, fmt.Errorf("drift check: decode current baseline: %w", err)
	}
	report := DiffSnapshots(existing, candidate)
	outcome.Report = &report
	outcome.Diverged = report.HasDiverged()
	outcome.Summary = report.SummaryString()
	store.RecordCheck(capture.DriftCheck{Timestamp: startedAt, Upstream: upstream, Diverged: outcome.Diverged, Summary: outcome.Summary})
	return outcome, nil
}
