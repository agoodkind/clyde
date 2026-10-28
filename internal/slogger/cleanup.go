package slogger

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type cleanupFile struct {
	path    string
	group   string
	size    int64
	modTime time.Time
}

type cleanupClock interface {
	Now() time.Time
}

type cleanupClockFunc func() time.Time

func (fn cleanupClockFunc) Now() time.Time {
	return fn()
}

var defaultCleanupClock cleanupClock = cleanupClockFunc(time.Now)

func cleanupPolicyNow() time.Time {
	return defaultCleanupClock.Now()
}

// CleanupResult carries the typed completion summary of a cleanup pass over
// the configured Clyde log roots. The event slogger.cleanup.completed mirrors
// these fields one-for-one.
type CleanupResult struct {
	// RetainedOpenBytes counts unlinked bytes that this pass cannot reclaim.
	RetainedOpenBytes int64 `json:"retained_open_bytes"`
	// RetainedOpenFiles counts unique unlinked log files still open.
	RetainedOpenFiles int `json:"retained_open_files"`
	// ProtectedBytes counts files that policy excludes from changes.
	ProtectedBytes int64 `json:"protected_bytes"`
	// ExcessBytes counts named and open unlinked bytes above the combined budget.
	ExcessBytes int64 `json:"excess_bytes"`
	// BytesBefore counts discovered log bytes before retention.
	BytesBefore int64 `json:"bytes_before"`
	// BytesAfter counts retained bytes after successful mutations.
	BytesAfter int64 `json:"bytes_after"`
	// Trimmed counts active files compacted without replacing their inode.
	Trimmed int `json:"trimmed"`
	// BytesTrimmed counts bytes removed by active-file compaction.
	BytesTrimmed int64 `json:"bytes_trimmed"`
	// TrimmedPaths lists active files changed in place.
	TrimmedPaths []string `json:"trimmed_paths"`
	// DeletedPaths lists archives removed successfully.
	DeletedPaths []string `json:"deleted_paths"`
	// ScannedRoots lists the cleanup roots walked during this pass. It is a
	// slice even when a single root is configured, so consumers can treat the
	// shape uniformly.
	ScannedRoots []string `json:"scanned_roots"`
	// Candidates counts files selected for removal or compaction.
	Candidates int `json:"candidates"`
	// Deleted counts files the cleanup pass actually removed.
	Deleted int `json:"deleted"`
	// BytesDeleted sums the sizes of removed files in bytes.
	BytesDeleted int64 `json:"bytes_deleted"`
	// Skipped lists paths that cleanup could not remove or compact.
	Skipped []string `json:"skipped"`
	// Errors lists "<path>: <err>" strings for each candidate the cleanup
	// pass attempted to remove but could not. Walk and collect errors also
	// appear here so the completed event remains the full failure record.
	Errors []string `json:"errors"`
	// DurationMS is the wall-clock duration of the cleanup pass in milliseconds.
	DurationMS int64 `json:"duration_ms"`
}

// RunCleanupOnce runs one pass of the slogger file cleanup walker against the
// provided policy and returns the typed result together with any walk or
// removal error. It is the exported entry point the daemon's periodic cleanup
// loop calls; the loop owns scheduling, this function owns one pass.
//
// The result mirrors the slogger.cleanup.completed slog event one-for-one, so
// callers that want to log or aggregate cleanup outcomes can do so without
// re-parsing the slog stream.
func RunCleanupOnce(policy CleanupPolicy) (CleanupResult, error) {
	return applyCleanupPolicy(policy)
}

// RunCleanupOnceContext applies one retention pass with the caller's context.
func RunCleanupOnceContext(ctx context.Context, policy CleanupPolicy) (CleanupResult, error) {
	return applyCleanupPolicyContext(ctx, policy)
}

func applyCleanupPolicy(policy CleanupPolicy) (CleanupResult, error) {
	return applyCleanupPolicyContext(context.Background(), policy)
}

func applyCleanupPolicyContext(ctx context.Context, policy CleanupPolicy) (CleanupResult, error) {
	emptyResult := CleanupResult{
		RetainedOpenBytes: 0,
		RetainedOpenFiles: 0,
		ProtectedBytes:    0,
		ExcessBytes:       0,
		BytesBefore:       0,
		BytesAfter:        0,
		Trimmed:           0,
		BytesTrimmed:      0,
		TrimmedPaths:      []string{},
		DeletedPaths:      []string{},
		ScannedRoots:      []string{},
		Candidates:        0,
		Deleted:           0,
		BytesDeleted:      0,
		Skipped:           []string{},
		Errors:            []string{},
		DurationMS:        0,
	}
	if !policy.Enabled {
		slog.DebugContext(ctx, "slogger.cleanup.skipped", "component", "slogger", "reason", "disabled")
		return emptyResult, nil
	}
	root := strings.TrimSpace(policy.Root)
	if root == "" && len(policy.Roots) == 0 && len(policy.Paths) == 0 {
		return emptyResult, errors.New("cleanup requires an owned root or file path")
	}
	scannedRoots := append([]string(nil), policy.Roots...)
	if root != "" {
		scannedRoots = append(scannedRoots, root)
	}
	startedAt := cleanupPolicyNow()
	slog.DebugContext(ctx, "slogger.cleanup.started",
		"component", "slogger",
		"root", root,
		"scanned_roots", scannedRoots,
	)
	result := CleanupResult{
		RetainedOpenBytes: policy.RetainedOpenBytes,
		RetainedOpenFiles: policy.RetainedOpenFiles,
		ProtectedBytes:    0,
		ExcessBytes:       0,
		BytesBefore:       0,
		BytesAfter:        0,
		Trimmed:           0,
		BytesTrimmed:      0,
		TrimmedPaths:      []string{},
		DeletedPaths:      []string{},
		ScannedRoots:      scannedRoots,
		Candidates:        0,
		Deleted:           0,
		BytesDeleted:      0,
		Skipped:           []string{},
		Errors:            []string{},
		DurationMS:        0,
	}
	files, walkErr := collectPolicyCleanupFiles(policy)
	if walkErr != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", root, walkErr))
	}
	retainCleanupFiles(files, policy, &result)
	duration := max(cleanupPolicyNow().Sub(startedAt), time.Millisecond)
	result.DurationMS = duration.Milliseconds()
	completionLevel := slog.LevelDebug
	if result.Deleted > 0 || result.Trimmed > 0 || len(result.Errors) > 0 {
		completionLevel = slog.LevelInfo
	}
	slog.Log(ctx, completionLevel, "slogger.cleanup.completed",
		"component", "slogger",
		"root", root,
		"scanned_roots", result.ScannedRoots,
		"candidates", result.Candidates,
		"deleted", result.Deleted,
		"bytes_deleted", result.BytesDeleted,
		"bytes_before", result.BytesBefore,
		"bytes_after", result.BytesAfter,
		"retained_open_bytes", result.RetainedOpenBytes,
		"retained_open_files", result.RetainedOpenFiles,
		"protected_bytes", result.ProtectedBytes,
		"excess_bytes", result.ExcessBytes,
		"trimmed", result.Trimmed,
		"bytes_trimmed", result.BytesTrimmed,
		"trimmed_paths", result.TrimmedPaths,
		"deleted_paths", result.DeletedPaths,
		"skipped", result.Skipped,
		"errors", result.Errors,
		"duration_ms", result.DurationMS,
	)
	if walkErr != nil {
		return result, walkErr
	}
	if len(result.Errors) > 0 {
		return result, fmt.Errorf("cleanup encountered %d removal errors under %s", len(result.Errors), root)
	}
	return result, nil
}

func collectCleanupFiles(root string) ([]cleanupFile, error) {
	files := make([]cleanupFile, 0)
	var failures []error
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			slog.Warn("slogger.cleanup.walk_entry_failed", "component", "slogger", "path", path, "err", err)
			failures = append(failures, fmt.Errorf("walk %s: %w", path, err))
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !isCleanupLogPath(path) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			slog.Warn("slogger.cleanup.stat_failed", "component", "slogger", "path", path, "err", infoErr)
			failures = append(failures, fmt.Errorf("stat %s: %w", path, infoErr))
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, cleanupFile{
			path:    path,
			group:   cleanupGroup(path),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		return nil
	})
	if walkErr != nil {
		failures = append(failures, fmt.Errorf("collect cleanup files under %s: %w", root, walkErr))
	}
	return files, errors.Join(failures...)
}

func selectCleanupCandidates(files []cleanupFile, policy CleanupPolicy, now time.Time) []cleanupFile {
	selected := make(map[string]cleanupFile)
	if policy.MaxAgeDays > 0 {
		cutoff := now.Add(-time.Duration(policy.MaxAgeDays) * 24 * time.Hour)
		for _, file := range files {
			if file.modTime.Before(cutoff) {
				selected[file.path] = file
			}
		}
	}
	if policy.MaxBackups > 0 {
		for _, file := range selectBackupOverflow(files, policy.MaxBackups) {
			selected[file.path] = file
		}
	}
	if policy.MaxTotalMB > 0 {
		maxBytes := int64(policy.MaxTotalMB) * 1024 * 1024
		for _, file := range selectTotalOverflow(files, maxBytes) {
			selected[file.path] = file
		}
	}
	out := make([]cleanupFile, 0, len(selected))
	for _, file := range selected {
		out = append(out, file)
	}
	sort.Slice(out, func(i int, j int) bool { return out[i].path < out[j].path })
	return out
}

func selectBackupOverflow(files []cleanupFile, maxBackups int) []cleanupFile {
	byGroup := make(map[string][]cleanupFile)
	for _, file := range files {
		byGroup[file.group] = append(byGroup[file.group], file)
	}
	selected := make([]cleanupFile, 0)
	for _, groupFiles := range byGroup {
		sort.Slice(groupFiles, func(i int, j int) bool {
			return groupFiles[i].modTime.After(groupFiles[j].modTime)
		})
		if len(groupFiles) <= maxBackups {
			continue
		}
		selected = append(selected, groupFiles[maxBackups:]...)
	}
	return selected
}

func selectTotalOverflow(files []cleanupFile, maxBytes int64) []cleanupFile {
	sortedFiles := append([]cleanupFile(nil), files...)
	sort.Slice(sortedFiles, func(i int, j int) bool {
		return sortedFiles[i].modTime.After(sortedFiles[j].modTime)
	})
	var total int64
	selected := make([]cleanupFile, 0)
	for _, file := range sortedFiles {
		total += file.size
		if total > maxBytes {
			selected = append(selected, file)
		}
	}
	return selected
}

func isCleanupLogPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if strings.HasSuffix(base, ".lock") || base == "lock" {
		return false
	}
	if _, archived := cleanupArchiveGroup(path); archived {
		return true
	}
	return strings.Contains(base, ".jsonl") || strings.HasSuffix(base, ".log") || strings.HasSuffix(base, ".log.gz") || strings.HasSuffix(base, ".raw")
}

func cleanupGroup(path string) string {
	if group, archived := cleanupArchiveGroup(path); archived {
		return group
	}
	base := filepath.Base(path)
	lower := strings.ToLower(base)
	for _, marker := range []string{".jsonl", ".log"} {
		index := strings.Index(lower, marker)
		if index >= 0 {
			return filepath.Join(filepath.Dir(path), base[:index]+marker)
		}
	}
	return path
}
