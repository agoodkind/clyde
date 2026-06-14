package mitm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
)

// BaselineRefreshOptions is part of Clyde's typed adapter surface.
type BaselineRefreshOptions struct {
	Upstream        string
	CaptureRoot     string
	BaselineRoot    string
	Reference       string
	DriftLogPath    string
	IncludeUA       []string
	ExcludeUA       []string
	RequireBodyKeys []string
	ForbidBodyKeys  []string
	Log             *slog.Logger
}

// BaselineRefreshOutcome is part of Clyde's typed adapter surface.
type BaselineRefreshOutcome struct {
	DriftOutcome
	BaselinePath string `json:"baseline_path"`
	Created      bool   `json:"created"`
	Updated      bool   `json:"updated"`
}

// RefreshBaseline is part of Clyde's typed adapter surface.
func RefreshBaseline(ctx context.Context, opts BaselineRefreshOptions) (BaselineRefreshOutcome, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	upstream := strings.TrimSpace(opts.Upstream)
	if upstream == "" {
		return BaselineRefreshOutcome{}, fmt.Errorf("baseline refresh: upstream is required")
	}
	captureRoot := strings.TrimSpace(opts.CaptureRoot)
	if captureRoot == "" {
		captureRoot = DefaultCaptureRoot()
	}
	transcriptPath, err := ResolveTranscriptPath(captureRoot, upstream)
	if err != nil {
		log.WarnContext(ctx, "mitm.baseline.transcript_resolve_failed", "concern", "providers.mitm.wire", "component", "mitm",
			"upstream", upstream,
			"capture_root", captureRoot,
			"err", err,
		)
		return BaselineRefreshOutcome{}, err
	}
	baselinePath := resolveBaselinePath(opts)
	if err := os.MkdirAll(filepath.Dir(baselinePath), 0o755); err != nil {
		log.WarnContext(ctx, "mitm.baseline.mkdir_failed", "concern", "providers.mitm.wire", "component", "mitm",
			"path", filepath.Dir(baselinePath),
			"err", err,
		)
		return BaselineRefreshOutcome{}, fmt.Errorf("baseline refresh mkdir: %w", err)
	}

	versionTag := "live-" + clock.Now().UTC().Format("20060102T150405")
	outcome := BaselineRefreshOutcome{
		DriftOutcome: DriftOutcome{
			Upstream:       upstream,
			ReferencePath:  baselinePath,
			TranscriptPath: transcriptPath,
			StartedAt:      clock.Now().UTC(), Timestamp: time.
					Time{},

			SchemaVersion: "", Diverged: false, Summary: "", V2: nil,
		},
		BaselinePath: baselinePath, Created: false, Updated: false,
	}
	result, refErr := refreshBaselineV2(log, outcome, opts, versionTag, transcriptPath, baselinePath)
	if errors.Is(refErr, errNoSnapshotRecords) {
		// The resolved per-concern wire transcript holds request-story leg
		// records, not the http_request / ws_* CaptureRecord kinds the snapshot
		// extractor needs, so an empty extraction is expected on live data.
		// Skip the tick without surfacing it as an infrastructure failure.
		log.WarnContext(ctx, "mitm.baseline.no_usable_records", "concern", "providers.mitm.wire", "component", "mitm",
			"upstream", upstream,
			"transcript", transcriptPath,
		)
		return result, nil
	}
	return result, refErr
}

func refreshBaselineV2(log *slog.Logger, outcome BaselineRefreshOutcome, opts BaselineRefreshOptions, versionTag, transcriptPath, baselinePath string) (BaselineRefreshOutcome, error) {
	candidate, err := ExtractSnapshotV2(transcriptPath, SnapshotV2Options{
		UpstreamName:               opts.Upstream,
		UpstreamVersion:            versionTag,
		ProviderFilter:             ProviderForUpstream(opts.Upstream),
		IncludeUserAgentSubstrings: opts.IncludeUA,
		ExcludeUserAgentSubstrings: opts.ExcludeUA,
		RequireBodyKeys:            opts.RequireBodyKeys,
		ForbidBodyKeys:             opts.ForbidBodyKeys, MaxBodyDepth: 0, EnumThreshold: 0,
	})
	if err != nil {
		return outcome, err
	}
	outcome.SchemaVersion = "v2"
	if existing, err := LoadSnapshotV2TOML(baselinePath); err == nil {
		report := DiffSnapshotsV2(existing, candidate)
		outcome.V2 = &report
		outcome.Diverged = report.HasDiverged()
		outcome.Summary = report.SummaryString()
		if !outcome.Diverged {
			return outcome, nil
		}
		outcome.Updated = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return outcome, fmt.Errorf("load baseline v2: %w", err)
	} else {
		outcome.Created = true
		outcome.Updated = true
		outcome.Summary = "initialized local v2 baseline for upstream=" + opts.Upstream
	}
	if opts.DriftLogPath != "" {
		if err := AppendDriftOutcome(opts.DriftLogPath, outcome.DriftOutcome); err != nil {
			log.Warn("mitm.baseline.drift_log_append_failed", "concern", "providers.mitm.wire", "path", opts.DriftLogPath, "err", err)
		}
	}
	if err := writeSnapshotV2Atomic(candidate, baselinePath); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// ResolveTranscriptPath returns the JSONL transcript the drift loop reads when
// extracting a live wire snapshot for upstream. The transcript source is the
// per-upstream drift capture file (<captureRoot>/drift/<upstream>.jsonl) the
// drift-capture writer appends one `http_request` [CaptureRecord] line to for
// every native request the MITM forwards. Those records carry request_headers,
// the request_body field-set summary, and (for claude) the billing attestation,
// which is exactly the shape the snapshot extractor consumes. The captureRoot is
// the same root the drift writer uses; upstream selects the per-upstream file.
//
// When the drift file does not exist yet (no native traffic has been captured
// since the loop started), the error is the graceful-skip signal; callers must
// degrade rather than treat a missing drift file as an infrastructure failure.
func ResolveTranscriptPath(captureRoot, upstream string) (string, error) {
	path := driftCapturePath(captureRoot, upstream)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path, nil
	}
	return "", fmt.Errorf("no MITM drift capture transcript at %s", path)
}

// ProviderForUpstream is part of Clyde's typed adapter surface.
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

// DefaultCaptureRoot is part of Clyde's typed adapter surface.
func DefaultCaptureRoot() string {
	return filepath.Join(config.DefaultStateDir(), "mitm")
}

func resolveBaselinePath(opts BaselineRefreshOptions) string {
	if path := expandHome(strings.TrimSpace(opts.Reference)); path != "" {
		return path
	}
	root := strings.TrimSpace(opts.BaselineRoot)
	if root == "" {
		root = DefaultBaselineRoot()
	}
	if existing, err := FindBaselineReference(root, opts.Upstream); err == nil {
		return existing
	}
	return BaselineReferencePath(root, opts.Upstream)
}

func writeSnapshotV2Atomic(snap SnapshotV2, baselinePath string) error {
	dir := filepath.Dir(baselinePath)
	tmpDir, err := os.MkdirTemp(dir, "baseline-v2-")
	if err != nil {
		slog.Warn("mitm.baseline.v2_temp_dir_failed", "concern", "providers.mitm.wire", "component", "mitm",
			"dir", dir,
			"err", err,
		)
		return fmt.Errorf("baseline temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	written, err := WriteSnapshotV2TOML(snap, tmpDir)
	if err != nil {
		return fmt.Errorf("write v2 snapshot temp file: %w", err)
	}
	if err := os.Rename(written, baselinePath); err != nil {
		return fmt.Errorf("install v2 baseline %s: %w", baselinePath, err)
	}
	return nil
}
