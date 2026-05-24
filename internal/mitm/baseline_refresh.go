package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"goodkind.io/clyde/internal/config"
)

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

type BaselineRefreshOutcome struct {
	DriftOutcome
	BaselinePath string `json:"baseline_path"`
	Created      bool   `json:"created"`
	Updated      bool   `json:"updated"`
}

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
		log.WarnContext(ctx, "mitm.baseline.transcript_resolve_failed",
			"component", "mitm",
			"upstream", upstream,
			"capture_root", captureRoot,
			"err", err,
		)
		return BaselineRefreshOutcome{}, err
	}
	baselinePath, useV2 := resolveBaselinePath(opts)
	if err := os.MkdirAll(filepath.Dir(baselinePath), 0o755); err != nil {
		log.WarnContext(ctx, "mitm.baseline.mkdir_failed",
			"component", "mitm",
			"path", filepath.Dir(baselinePath),
			"err", err,
		)
		return BaselineRefreshOutcome{}, fmt.Errorf("baseline refresh mkdir: %w", err)
	}

	versionTag := "live-" + currentTime().UTC().Format("20060102T150405")
	outcome := BaselineRefreshOutcome{
		DriftOutcome: DriftOutcome{
			Upstream:       upstream,
			ReferencePath:  baselinePath,
			TranscriptPath: transcriptPath,
			StartedAt:      currentTime().UTC(),
		},
		BaselinePath: baselinePath,
	}
	if useV2 {
		return refreshBaselineV2(log, outcome, opts, versionTag, transcriptPath, baselinePath)
	}
	return refreshBaselineV1(log, outcome, opts, versionTag, transcriptPath, baselinePath)
}

func refreshBaselineV2(log *slog.Logger, outcome BaselineRefreshOutcome, opts BaselineRefreshOptions, versionTag, transcriptPath, baselinePath string) (BaselineRefreshOutcome, error) {
	candidate, err := ExtractSnapshotV2(transcriptPath, SnapshotV2Options{
		UpstreamName:               opts.Upstream,
		UpstreamVersion:            versionTag,
		ProviderFilter:             ProviderForUpstream(opts.Upstream),
		IncludeUserAgentSubstrings: opts.IncludeUA,
		ExcludeUserAgentSubstrings: opts.ExcludeUA,
		RequireBodyKeys:            opts.RequireBodyKeys,
		ForbidBodyKeys:             opts.ForbidBodyKeys,
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
	} else if !os.IsNotExist(err) {
		return outcome, fmt.Errorf("load baseline v2: %w", err)
	} else {
		outcome.Created = true
		outcome.Updated = true
		outcome.Summary = fmt.Sprintf("initialized local v2 baseline for upstream=%s", opts.Upstream)
	}
	if opts.DriftLogPath != "" {
		if err := AppendDriftOutcome(opts.DriftLogPath, outcome.DriftOutcome); err != nil {
			log.Warn("mitm.baseline.drift_log_append_failed", "path", opts.DriftLogPath, "err", err)
		}
	}
	if err := writeSnapshotV2Atomic(candidate, baselinePath); err != nil {
		return outcome, err
	}
	return outcome, nil
}

func refreshBaselineV1(log *slog.Logger, outcome BaselineRefreshOutcome, opts BaselineRefreshOptions, versionTag, transcriptPath, baselinePath string) (BaselineRefreshOutcome, error) {
	candidate, err := ExtractSnapshot(transcriptPath, SnapshotOptions{
		UpstreamName:    opts.Upstream,
		UpstreamVersion: versionTag,
		ProviderFilter:  ProviderForUpstream(opts.Upstream),
	})
	if err != nil {
		return outcome, err
	}
	outcome.SchemaVersion = "v1"
	if existing, err := LoadSnapshotTOML(baselinePath); err == nil {
		report := DiffSnapshots(existing, candidate)
		outcome.V1 = &report
		outcome.Diverged = report.HasDiverged()
		outcome.Summary = report.SummaryString()
		if !outcome.Diverged {
			return outcome, nil
		}
		outcome.Updated = true
	} else if !os.IsNotExist(err) {
		return outcome, fmt.Errorf("load baseline v1: %w", err)
	} else {
		outcome.Created = true
		outcome.Updated = true
		outcome.Summary = fmt.Sprintf("initialized local v1 baseline for upstream=%s", opts.Upstream)
	}
	if opts.DriftLogPath != "" {
		if err := AppendDriftOutcome(opts.DriftLogPath, outcome.DriftOutcome); err != nil {
			log.Warn("mitm.baseline.drift_log_append_failed", "path", opts.DriftLogPath, "err", err)
		}
	}
	if err := writeSnapshotV1Atomic(candidate, baselinePath); err != nil {
		return outcome, err
	}
	return outcome, nil
}

func ResolveTranscriptPath(captureRoot, upstream string) (string, error) {
	root := expandHome(strings.TrimSpace(captureRoot))
	if root == "" {
		return "", fmt.Errorf("capture root is required")
	}
	for _, candidate := range []string{
		filepath.Join(root, "capture.jsonl"),
		filepath.Join(root, strings.TrimSpace(upstream), "capture.jsonl"),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	upstreamDir := filepath.Join(root, strings.TrimSpace(upstream))
	entries, err := os.ReadDir(upstreamDir)
	if err != nil {
		return "", fmt.Errorf("no capture transcript for %q under %s", upstream, root)
	}
	type candidateDir struct {
		path string
		name string
	}
	var candidates []candidateDir
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(upstreamDir, entry.Name(), "capture.jsonl")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			candidates = append(candidates, candidateDir{path: path, name: entry.Name()})
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no capture transcript for %q under %s", upstream, root)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name > candidates[j].name })
	return candidates[0].path, nil
}

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

func DefaultUseV2Baseline(upstream string) bool {
	return ProviderForUpstream(upstream) != "codex"
}

func DefaultCaptureRoot() string {
	return filepath.Join(config.DefaultStateDir(), "mitm")
}

func resolveBaselinePath(opts BaselineRefreshOptions) (string, bool) {
	if path := expandHome(strings.TrimSpace(opts.Reference)); path != "" {
		return path, isV2SnapshotFile(path) || strings.HasSuffix(path, "reference-v2.toml")
	}
	root := strings.TrimSpace(opts.BaselineRoot)
	if root == "" {
		root = DefaultBaselineRoot()
	}
	if existing, err := FindBaselineReference(root, opts.Upstream); err == nil {
		return existing, isV2SnapshotFile(existing) || strings.HasSuffix(existing, "reference-v2.toml")
	}
	useV2 := DefaultUseV2Baseline(opts.Upstream)
	return BaselineReferencePath(root, opts.Upstream, useV2), useV2
}

func writeSnapshotV2Atomic(snap SnapshotV2, baselinePath string) error {
	dir := filepath.Dir(baselinePath)
	tmpDir, err := os.MkdirTemp(dir, "baseline-v2-")
	if err != nil {
		slog.Warn("mitm.baseline.v2_temp_dir_failed",
			"component", "mitm",
			"dir", dir,
			"err", err,
		)
		return fmt.Errorf("baseline temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	written, err := WriteSnapshotV2TOML(snap, tmpDir)
	if err != nil {
		return err
	}
	return os.Rename(written, baselinePath)
}

func writeSnapshotV1Atomic(snap Snapshot, baselinePath string) error {
	dir := filepath.Dir(baselinePath)
	tmpDir, err := os.MkdirTemp(dir, "baseline-v1-")
	if err != nil {
		slog.Warn("mitm.baseline.v1_temp_dir_failed",
			"component", "mitm",
			"dir", dir,
			"err", err,
		)
		return fmt.Errorf("baseline temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	written, err := WriteSnapshotTOML(snap, tmpDir)
	if err != nil {
		return err
	}
	return os.Rename(written, baselinePath)
}
