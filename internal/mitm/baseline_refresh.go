package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/mitm/capture"
)

// baselineShapeWindow bounds how far back the refresh reads deduped shapes when
// building a candidate snapshot. Shapes last seen before now-window are
// excluded so a long-stale shape does not keep a retired wire flavor alive in
// the baseline. The corpus itself is held forever; this only scopes one
// refresh's input.
const baselineShapeWindow = 7 * 24 * time.Hour

// baselineDiffCategory is the typed bucket one flattened baseline difference
// belongs to. Every [capture.BaselineDiff] row carries one of these, so the
// difference matrix stays a closed, queryable set rather than free-form text.
type baselineDiffCategory string

const (
	baselineDiffHeaderMissing      baselineDiffCategory = "header_missing"
	baselineDiffHeaderExtra        baselineDiffCategory = "header_extra"
	baselineDiffHeaderClassChanged baselineDiffCategory = "header_class_changed"
	baselineDiffHeaderValuesDiff   baselineDiffCategory = "header_values_diff"
	baselineDiffBodyMissing        baselineDiffCategory = "body_missing"
	baselineDiffBodyExtra          baselineDiffCategory = "body_extra"
	baselineDiffBetaFingerprint    baselineDiffCategory = "beta_fingerprint"
	baselineDiffUserAgent          baselineDiffCategory = "user_agent"
)

// BaselineRefreshOptions configures one upstream's baseline refresh from the
// capture store's deduped shape corpus. The keep filters scope which captured
// caller flavor the candidate snapshot is built from.
type BaselineRefreshOptions struct {
	Upstream        string
	IncludeUA       []string
	ExcludeUA       []string
	RequireBodyKeys []string
	ForbidBodyKeys  []string
	Log             *slog.Logger
}

// BaselineRefreshOutcome reports the outcome of one baseline refresh.
type BaselineRefreshOutcome struct {
	DriftOutcome
	Created bool `json:"created"`
	Updated bool `json:"updated"`
}

// RefreshBaseline rebuilds the candidate snapshot for one upstream from the
// store's deduped shape corpus, diffs it against the current stored baseline,
// and on a change (or first baseline) folds the differences into the deduped
// difference matrix and writes the new authoritative baseline. Every run
// records a drift-check outcome. The store is the single source of truth; no
// file is read or written.
func RefreshBaseline(ctx context.Context, store *capture.Store, opts BaselineRefreshOptions) (BaselineRefreshOutcome, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	upstream := strings.TrimSpace(opts.Upstream)
	if upstream == "" {
		return BaselineRefreshOutcome{}, fmt.Errorf("baseline refresh: upstream is required")
	}
	if store == nil {
		return BaselineRefreshOutcome{}, fmt.Errorf("baseline refresh: capture store is required")
	}

	startedAt := clock.Now().UTC()
	outcome := BaselineRefreshOutcome{
		DriftOutcome: DriftOutcome{
			Upstream:  upstream,
			StartedAt: startedAt,
			Timestamp: startedAt,
			Diverged:  false,
			Summary:   "",
			Report:    nil,
		},
		Created: false,
		Updated: false,
	}

	since := startedAt.Add(-baselineShapeWindow)
	shapes, err := store.ShapesForUpstream(ctx, upstream, since)
	if err != nil {
		return outcome, fmt.Errorf("baseline refresh: read shapes: %w", err)
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
		// An empty corpus is the expected cold-start case (no native traffic
		// captured yet). Record the check and skip without surfacing an
		// infrastructure failure.
		// A no-op refresh (empty/filtered corpus) records no check row. The
		// periodic sweep owns steady-state cadence; the per-request refresher
		// records only meaningful changes, so its ~2s ticks do not pile up.
		log.WarnContext(ctx, "mitm.baseline.no_usable_shapes", "concern", "providers.mitm.wire", "component", "mitm",
			"upstream", upstream,
			"err", err,
		)
		return outcome, nil
	}

	existingRaw, hasExisting, err := store.CurrentBaseline(ctx, upstream)
	if err != nil {
		return outcome, fmt.Errorf("baseline refresh: read current baseline: %w", err)
	}

	if hasExisting {
		existing, err := unmarshalSnapshot(existingRaw)
		if err != nil {
			return outcome, fmt.Errorf("baseline refresh: decode current baseline: %w", err)
		}
		report := DiffSnapshots(existing, candidate)
		outcome.Report = &report
		outcome.Diverged = report.HasDiverged()
		outcome.Summary = report.SummaryString()
		if !outcome.Diverged {
			// No drift: the per-request refresher records no steady-state check
			// row, so its ~2s ticks do not accumulate. The periodic sweep keeps
			// recording cadence.
			return outcome, nil
		}
		store.RecordCheck(capture.DriftCheck{Timestamp: startedAt, Upstream: upstream, Diverged: true, Summary: outcome.Summary})
		outcome.Updated = true
		if err := putBaseline(ctx, store, upstream, candidate, report, startedAt); err != nil {
			return outcome, err
		}
		return outcome, nil
	}

	outcome.Created = true
	outcome.Updated = true
	outcome.Summary = "initialized baseline for upstream=" + upstream
	store.RecordCheck(capture.DriftCheck{Timestamp: startedAt, Upstream: upstream, Diverged: false, Summary: outcome.Summary})
	if err := putBaseline(ctx, store, upstream, candidate, DiffReport{Upstream: upstream, MissingFlavors: nil, ExtraFlavors: nil, FlavorReports: nil}, startedAt); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// putBaseline serializes the candidate snapshot, flattens the diff report into
// the deduped difference matrix, and persists both through one
// [capture.Store.PutBaseline] transaction.
func putBaseline(ctx context.Context, store *capture.Store, upstream string, candidate Snapshot, report DiffReport, ts time.Time) error {
	snapshotJSON, err := marshalSnapshot(candidate)
	if err != nil {
		slog.WarnContext(ctx, "mitm.baseline.encode_candidate_failed", "concern", "providers.mitm.wire", "upstream", upstream, "err", err)
		return fmt.Errorf("baseline refresh: encode candidate: %w", err)
	}
	diffs := flattenDiffReport(upstream, report, ts)
	change := capture.BaselineChange{
		Timestamp: ts,
		Upstream:  upstream,
		Snapshot:  snapshotJSON,
		Diffs:     diffs,
	}
	if err := store.PutBaseline(ctx, change); err != nil {
		slog.WarnContext(ctx, "mitm.baseline.put_baseline_failed", "concern", "providers.mitm.wire", "upstream", upstream, "err", err)
		return fmt.Errorf("baseline refresh: put baseline: %w", err)
	}
	return nil
}

// flattenDiffReport turns a structured [DiffReport] into the flat list of
// [capture.BaselineDiff] rows the difference matrix dedupes on. Each
// per-flavor mismatch across the header, body, and signature categories
// becomes one row.
func flattenDiffReport(upstream string, report DiffReport, ts time.Time) []capture.BaselineDiff {
	var out []capture.BaselineDiff
	for _, fr := range report.FlavorReports {
		out = append(out, flavorDiffs(upstream, fr, ts)...)
	}
	return out
}

func flavorDiffs(upstream string, fr FlavorDiffReport, ts time.Time) []capture.BaselineDiff {
	var out []capture.BaselineDiff
	if fr.UserAgentChange != nil {
		out = append(out, mismatchDiff(upstream, fr.Slug, baselineDiffUserAgent, *fr.UserAgentChange, ts))
	}
	if fr.BetaFingerprintChange != nil {
		out = append(out, mismatchDiff(upstream, fr.Slug, baselineDiffBetaFingerprint, *fr.BetaFingerprintChange, ts))
	}
	for _, name := range fr.HeaderMissing {
		out = append(out, fieldDiff(upstream, fr.Slug, baselineDiffHeaderMissing, name, "header missing in candidate", ts))
	}
	for _, name := range fr.HeaderExtra {
		out = append(out, fieldDiff(upstream, fr.Slug, baselineDiffHeaderExtra, name, "header extra in candidate", ts))
	}
	for _, m := range fr.HeaderClassChanged {
		out = append(out, mismatchDiff(upstream, fr.Slug, baselineDiffHeaderClassChanged, m, ts))
	}
	for _, m := range fr.HeaderValuesDiff {
		out = append(out, mismatchDiff(upstream, fr.Slug, baselineDiffHeaderValuesDiff, m, ts))
	}
	for _, name := range fr.BodyMissing {
		out = append(out, fieldDiff(upstream, fr.Slug, baselineDiffBodyMissing, name, "body field missing in candidate", ts))
	}
	for _, name := range fr.BodyExtra {
		out = append(out, fieldDiff(upstream, fr.Slug, baselineDiffBodyExtra, name, "body field extra in candidate", ts))
	}
	return out
}

func mismatchDiff(upstream, flavor string, category baselineDiffCategory, m DiffMismatch, ts time.Time) capture.BaselineDiff {
	return capture.BaselineDiff{
		Upstream:  upstream,
		Flavor:    flavor,
		Category:  string(category),
		Field:     m.Field,
		Before:    m.Expected,
		After:     m.Got,
		Reason:    m.Reason,
		FirstSeen: ts,
		LastSeen:  ts,
		SeenCount: 1,
	}
}

// fieldDiff builds one presence-style difference row (header/body added or
// removed) where the before and after values are not meaningful, so both are
// recorded empty.
func fieldDiff(upstream, flavor string, category baselineDiffCategory, field, reason string, ts time.Time) capture.BaselineDiff {
	return capture.BaselineDiff{
		Upstream:  upstream,
		Flavor:    flavor,
		Category:  string(category),
		Field:     field,
		Before:    "",
		After:     "",
		Reason:    reason,
		FirstSeen: ts,
		LastSeen:  ts,
		SeenCount: 1,
	}
}

// ProviderForUpstream maps an upstream slug to its routed provider family. It
// is the inverse of the drift writer's provider->upstream mapping.
func ProviderForUpstream(upstream string) string {
	name := strings.ToLower(strings.TrimSpace(upstream))
	switch {
	case strings.HasPrefix(name, "claude-"):
		return "claude"
	case strings.HasPrefix(name, "codex-"):
		return "codex"
	}
	return ""
}
