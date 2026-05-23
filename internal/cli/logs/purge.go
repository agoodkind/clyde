package logs

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"goodkind.io/clyde/internal/config"
)

// PurgeMode is the typed mode the purge entrypoint runs in. The default mode
// is [PurgeModeDryRun]: it walks the state root and records what would be
// deleted without removing any file.
type PurgeMode string

const (
	// PurgeModeDryRun records the candidate purge plan without removing files.
	PurgeModeDryRun PurgeMode = "dry_run"
	// PurgeModeApply removes the identified legacy files.
	PurgeModeApply PurgeMode = "apply"
)

// PurgeOptions configures a purge pass over the Clyde state root.
type PurgeOptions struct {
	StateRoot string
	Mode      PurgeMode
	Now       time.Time
	Logging   config.LoggingConfig
	MITM      config.MITMConfig
}

// PurgeResult is the typed completion summary of a purge pass.
type PurgeResult struct {
	StateRoot      string    `json:"state_root"`
	Mode           PurgeMode `json:"mode"`
	Generated      time.Time `json:"generated"`
	ScannedRoots   []string  `json:"scanned_roots"`
	LegacyRoots    []string  `json:"legacy_roots"`
	DeletedPaths   []string  `json:"deleted_paths"`
	DeletedFiles   int       `json:"deleted_files"`
	DeletedBytes   int64     `json:"deleted_bytes"`
	PreservedPaths []string  `json:"preserved_paths"`
	SkippedPaths   []string  `json:"skipped_paths"`
	Errors         []string  `json:"errors"`
	DurationMS     int64     `json:"duration_ms"`
}

// IsOutputPayload marks PurgeResult as a valid output.Encoder payload variant.
// The method is intentionally empty; it exists so the closed output.Payload
// interface is satisfied without widening the marker.
func (PurgeResult) IsOutputPayload() {}

// purgeCandidate is a legacy-classified file collected during the walk pass.
// All filesystem operations (Stat, Remove) on candidates happen after the
// walk completes, so the walk callback itself does not perform race-prone
// filesystem operations on paths returned by the walk.
type purgeCandidate struct {
	relativePath string
	absolutePath string
	size         int64
}

// PurgeLegacyLogs walks the Clyde state root, identifies files matching
// legacy logging surfaces, and either reports or removes them based on
// [PurgeOptions.Mode]. It returns a typed [PurgeResult] and emits a single
// summary event via the structured logger.
//
// The implementation never deletes a file recognized by [isNewFormatPath] so
// the purge cannot drift into deleting current-format logs. The new-format
// file set is encoded explicitly there.
func PurgeLegacyLogs(options PurgeOptions) (PurgeResult, error) {
	stateRoot, err := normalizePurgeStateRoot(options.StateRoot)
	if err != nil {
		return PurgeResult{}, err
	}
	mode := options.Mode
	if mode == "" {
		mode = PurgeModeDryRun
	}
	startedAt := options.Now
	if startedAt.IsZero() {
		startedAt = cliLogsNow()
	}
	result := newPurgeResult(stateRoot, mode, startedAt)

	candidates, preserved, walkErrors, walkErr := collectPurgeCandidates(stateRoot)
	result.PreservedPaths = preserved
	result.Errors = append(result.Errors, walkErrors...)
	legacyRoots := collectLegacyRoots(candidates)
	for _, candidate := range candidates {
		applyPurgeCandidate(candidate, mode, &result)
	}
	finalizePurgeResult(&result, legacyRoots, startedAt)
	emitPurgeCompletedEvent(result)
	if walkErr != nil {
		slog.Warn(
			"cli.logs.purge.walk_failed",
			"component", "cli",
			"state_root", stateRoot,
			"err", walkErr,
		)
		return result, fmt.Errorf("purge walk under %s: %w", stateRoot, walkErr)
	}
	return result, nil
}

func normalizePurgeStateRoot(stateRoot string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(stateRoot))
	if cleaned == "." || cleaned == "" {
		slog.Warn("cli.logs.purge.state_root_missing", "component", "cli")
		return "", fmt.Errorf("state root is required")
	}
	return cleaned, nil
}

func newPurgeResult(stateRoot string, mode PurgeMode, startedAt time.Time) PurgeResult {
	return PurgeResult{
		StateRoot:      stateRoot,
		Mode:           mode,
		Generated:      startedAt,
		ScannedRoots:   []string{stateRoot},
		LegacyRoots:    []string{},
		DeletedPaths:   []string{},
		DeletedFiles:   0,
		DeletedBytes:   0,
		PreservedPaths: []string{},
		SkippedPaths:   []string{},
		Errors:         []string{},
		DurationMS:     0,
	}
}

func collectPurgeCandidates(stateRoot string) ([]purgeCandidate, []string, []string, error) {
	candidates := make([]purgeCandidate, 0)
	preserved := make([]string, 0)
	errors := make([]string, 0)
	walkErr := filepath.WalkDir(stateRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("cli.logs.purge.walk_entry_failed", "component", "cli", "path", path, "err", err)
			errors = append(errors, fmt.Sprintf("%s: %v", path, err))
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		relativePath, relErr := filepath.Rel(stateRoot, path)
		if relErr != nil {
			errors = append(errors, fmt.Sprintf("relative %s: %v", path, relErr))
			return nil
		}
		relSlash := filepath.ToSlash(relativePath)
		if isNewFormatPath(relSlash) {
			preserved = append(preserved, relSlash)
			return nil
		}
		if !isLegacyLogPath(relSlash) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			errors = append(errors, fmt.Sprintf("stat %s: %v", path, infoErr))
			return nil
		}
		candidates = append(candidates, purgeCandidate{
			relativePath: relSlash,
			absolutePath: path,
			size:         info.Size(),
		})
		return nil
	})
	sort.Strings(preserved)
	if walkErr != nil {
		walkErr = fmt.Errorf("walk state root %s: %w", stateRoot, walkErr)
	}
	return candidates, preserved, errors, walkErr
}

func collectLegacyRoots(candidates []purgeCandidate) []string {
	roots := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		roots[legacyRootFor(candidate.relativePath)] = struct{}{}
	}
	output := make([]string, 0, len(roots))
	for root := range roots {
		output = append(output, root)
	}
	sort.Strings(output)
	return output
}

func applyPurgeCandidate(candidate purgeCandidate, mode PurgeMode, result *PurgeResult) {
	if mode == PurgeModeDryRun {
		result.DeletedPaths = append(result.DeletedPaths, candidate.relativePath)
		result.DeletedFiles++
		result.DeletedBytes += candidate.size
		return
	}
	if err := os.Remove(candidate.absolutePath); err != nil {
		if os.IsNotExist(err) {
			return
		}
		slog.Warn("cli.logs.purge.remove_failed", "component", "cli", "path", candidate.absolutePath, "err", err)
		result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", candidate.relativePath, err))
		result.SkippedPaths = append(result.SkippedPaths, candidate.relativePath)
		return
	}
	result.DeletedPaths = append(result.DeletedPaths, candidate.relativePath)
	result.DeletedFiles++
	result.DeletedBytes += candidate.size
}

func finalizePurgeResult(result *PurgeResult, legacyRoots []string, startedAt time.Time) {
	result.LegacyRoots = legacyRoots
	sort.Strings(result.DeletedPaths)
	sort.Strings(result.SkippedPaths)
	duration := max(cliLogsNow().Sub(startedAt), time.Millisecond)
	result.DurationMS = duration.Milliseconds()
}

func emitPurgeCompletedEvent(result PurgeResult) {
	slog.Info("cli.logs.purge.completed",
		"component", "cli",
		"state_root", result.StateRoot,
		"mode", string(result.Mode),
		"scanned_roots", result.ScannedRoots,
		"legacy_roots", result.LegacyRoots,
		"deleted_files", result.DeletedFiles,
		"deleted_bytes", result.DeletedBytes,
		"skipped", result.SkippedPaths,
		"errors", result.Errors,
		"duration_ms", result.DurationMS,
	)
}

// isNewFormatPath returns true when the relative-to-state-root path names a
// file produced by the unified logging implementation. The new-format file
// set is encoded explicitly here so the purge cannot drift into deleting
// current-format logs.
func isNewFormatPath(relativePath string) bool {
	lower := strings.ToLower(relativePath)
	base := strings.ToLower(filepath.Base(lower))
	if strings.HasSuffix(base, ".lock") || base == "lock" {
		return true
	}
	if isPreRepairLog(lower, base) {
		return false
	}
	if isMITMRawCapture(lower) {
		return true
	}
	if isMITMCaptureIndex(lower, base) {
		return true
	}
	if isMITMProfileProcessLog(lower, base) {
		return true
	}
	if isSessionBackupArtifact(lower, base) {
		return true
	}
	if isInventoryIndexLog(lower) {
		return true
	}
	if isPerChatTranscriptLog(lower) {
		return true
	}
	if isConcernLog(lower) {
		return true
	}
	if isProviderSidecarLog(lower, base) {
		return true
	}
	if isTopLevelProcessLog(lower, base) {
		return true
	}
	return false
}

// isLegacyLogPath returns true when the relative-to-state-root path names a
// known legacy logging surface that the purge entrypoint may remove.
//
// The current legacy class is pre-repair retained logs: files whose base
// name contains the "pre-repair" marker, or files located under a directory
// named pre-repair. These files are produced by historical session-repair
// flows that the unified logging refactor no longer writes to.
//
// New legacy classes should add a positive-match branch here and document
// what they cover.
func isLegacyLogPath(relativePath string) bool {
	lower := strings.ToLower(relativePath)
	base := strings.ToLower(filepath.Base(lower))
	return isPreRepairLog(lower, base)
}

func isPreRepairLog(lowerPath string, base string) bool {
	if strings.Contains(base, "pre-repair") {
		return true
	}
	return strings.Contains(lowerPath, "/pre-repair/")
}

func legacyRootFor(relativePath string) string {
	directory := filepath.Dir(relativePath)
	if directory == "." {
		return relativePath
	}
	return filepath.ToSlash(directory)
}
