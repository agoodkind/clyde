package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/clyde/internal/clock"
)

// DriftCheckOptions configures one compare-only drift run against the
// current local capture store. UA / body-key filters scope which
// captured caller flavor the candidate snapshot is built from.
type DriftCheckOptions struct {
	Upstream        string
	Reference       string
	CaptureRoot     string
	CACertPath      string // kept for backward-compatible CLI/config shape; compare-only drift does not capture
	DriftLogPath    string
	IncludeUA       []string
	ExcludeUA       []string
	RequireBodyKeys []string
	ForbidBodyKeys  []string
	Log             *slog.Logger
}

// RunDriftCheck performs the snapshot + diff cycle for one upstream
// using the current local capture store and appends the structured
// outcome to DriftLogPath.
// Returns the outcome and a non-nil error on infrastructure failure.
// The outcome's Diverged field reports whether the snapshots
// disagreed; callers may want to escalate divergence separately from
// infrastructure failures.
func RunDriftCheck(ctx context.Context, opts DriftCheckOptions) (DriftOutcome, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if strings.TrimSpace(opts.Reference) == "" {
		return DriftOutcome{}, fmt.Errorf("reference path is required")
	}
	if strings.TrimSpace(opts.DriftLogPath) == "" {
		return DriftOutcome{}, fmt.Errorf("drift log path is required")
	}
	captureRoot := strings.TrimSpace(opts.CaptureRoot)
	if captureRoot == "" {
		captureRoot = DefaultCaptureRoot()
	}
	transcriptPath, err := ResolveTranscriptPath(captureRoot, opts.Upstream)
	if err != nil {
		opts.Log.WarnContext(ctx, "mitm.drift.transcript_resolve_failed", "concern", "providers.mitm.wire", "upstream", opts.Upstream,
			"capture_root", captureRoot,
			"err", err,
		)
		return DriftOutcome{}, err
	}
	startedAt := clock.Now().UTC()

	outcome := DriftOutcome{
		Upstream:       opts.Upstream,
		ReferencePath:  opts.Reference,
		TranscriptPath: transcriptPath,
		StartedAt:      startedAt, Timestamp: time.
				Time{},

		SchemaVersion: "", Diverged: false, Summary: "", V2: nil,
	}

	versionTag := "live-" + startedAt.Format("20060102T150405")
	if err := loadAndDiffSnapshotV2(ctx, opts, transcriptPath, versionTag, &outcome); err != nil {
		return outcome, err
	}

	if err := AppendDriftOutcome(opts.DriftLogPath, outcome); err != nil {
		opts.Log.WarnContext(ctx, "mitm.drift.log_append_failed", "concern", "providers.mitm.wire", "path", opts.DriftLogPath, "err", err)
	}
	// AppendDriftOutcome populates Diverged + Summary on the in-place
	// outcome. Re-derive here so callers that skip the log path still
	// see those fields populated on the returned value.
	if outcome.SchemaVersion == "v2" && outcome.V2 != nil {
		outcome.Diverged = outcome.V2.HasDiverged()
		outcome.Summary = outcome.V2.SummaryString()
	}
	return outcome, nil
}

// loadAndDiffSnapshotV2 loads a v2 reference snapshot, extracts a
// fresh candidate from the live transcript, diffs the two, and writes
// the result into outcome. Returns a wrapped error when either the
// reference or the candidate fail to materialize.
func loadAndDiffSnapshotV2(ctx context.Context, opts DriftCheckOptions, transcriptPath, versionTag string, outcome *DriftOutcome) error {
	ref, err := LoadSnapshotV2TOML(opts.Reference)
	if err != nil {
		opts.Log.WarnContext(ctx, "mitm.drift.load_v2_reference_failed", "concern", "providers.mitm.wire", "reference", opts.Reference,
			"err", err,
		)
		return fmt.Errorf("load v2 reference: %w", err)
	}
	cand, err := ExtractSnapshotV2(transcriptPath, SnapshotV2Options{
		UpstreamName:               opts.Upstream,
		UpstreamVersion:            versionTag,
		ProviderFilter:             ProviderForUpstream(opts.Upstream),
		IncludeUserAgentSubstrings: opts.IncludeUA,
		ExcludeUserAgentSubstrings: opts.ExcludeUA,
		RequireBodyKeys:            opts.RequireBodyKeys,
		ForbidBodyKeys:             opts.ForbidBodyKeys, MaxBodyDepth: 0, EnumThreshold: 0,
	})
	if err != nil {
		opts.Log.WarnContext(ctx, "mitm.drift.extract_v2_failed", "concern", "providers.mitm.wire", "transcript", transcriptPath,
			"upstream", opts.Upstream,
			"err", err,
		)
		return fmt.Errorf("extract v2: %w", err)
	}
	report := DiffSnapshotsV2(ref, cand)
	outcome.SchemaVersion = "v2"
	outcome.V2 = &report
	return nil
}
