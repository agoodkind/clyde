package slogger

import (
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
	// ScannedRoots lists the cleanup roots walked during this pass. It is a
	// slice even when a single root is configured, so consumers can treat the
	// shape uniformly.
	ScannedRoots []string `json:"scanned_roots"`
	// Candidates counts files identified as eligible for removal by the active
	// policy regardless of whether removal succeeded.
	Candidates int `json:"candidates"`
	// Deleted counts files the cleanup pass actually removed.
	Deleted int `json:"deleted"`
	// BytesDeleted sums the sizes of removed files in bytes.
	BytesDeleted int64 `json:"bytes_deleted"`
	// Skipped lists candidate paths the cleanup pass identified but did not
	// remove because os.Remove returned an error other than os.IsNotExist.
	Skipped []string `json:"skipped"`
	// Errors lists "<path>: <err>" strings for each candidate the cleanup
	// pass attempted to remove but could not. Walk and collect errors also
	// appear here so the completed event remains the full failure record.
	Errors []string `json:"errors"`
	// DurationMS is the wall-clock duration of the cleanup pass in milliseconds.
	DurationMS int64 `json:"duration_ms"`
}

func applyCleanupPolicy(policy CleanupPolicy) error {
	if !policy.Enabled {
		slog.Debug("slogger.cleanup.skipped", "component", "slogger", "reason", "disabled")
		return nil
	}
	root := strings.TrimSpace(policy.Root)
	if root == "" {
		slog.Debug("slogger.cleanup.skipped", "component", "slogger", "reason", "empty_root")
		return nil
	}
	if _, statErr := os.Stat(root); errors.Is(statErr, fs.ErrNotExist) {
		slog.Debug("slogger.cleanup.skipped",
			"component", "slogger",
			"reason", "root_missing",
			"root", root,
		)
		return nil
	}
	scannedRoots := []string{root}
	startedAt := cleanupPolicyNow()
	slog.Info("slogger.cleanup.started",
		"component", "slogger",
		"root", root,
		"scanned_roots", scannedRoots,
	)
	result := CleanupResult{
		ScannedRoots: scannedRoots,
		Candidates:   0,
		Deleted:      0,
		BytesDeleted: 0,
		Skipped:      []string{},
		Errors:       []string{},
		DurationMS:   0,
	}
	files, walkErr := collectCleanupFiles(root)
	if walkErr != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", root, walkErr))
	}
	candidates := selectCleanupCandidates(files, policy, cleanupPolicyNow())
	result.Candidates = len(candidates)
	for _, candidate := range candidates {
		if err := os.Remove(candidate.path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			slog.Warn("slogger.cleanup.remove_failed",
				"component", "slogger",
				"path", candidate.path,
				"err", err,
			)
			result.Skipped = append(result.Skipped, candidate.path)
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", candidate.path, err))
			continue
		}
		result.Deleted++
		result.BytesDeleted += candidate.size
	}
	duration := max(cleanupPolicyNow().Sub(startedAt), time.Millisecond)
	result.DurationMS = duration.Milliseconds()
	slog.Info("slogger.cleanup.completed",
		"component", "slogger",
		"root", root,
		"scanned_roots", result.ScannedRoots,
		"candidates", result.Candidates,
		"deleted", result.Deleted,
		"bytes_deleted", result.BytesDeleted,
		"skipped", result.Skipped,
		"errors", result.Errors,
		"duration_ms", result.DurationMS,
	)
	if walkErr != nil {
		return walkErr
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("cleanup encountered %d removal errors under %s", len(result.Errors), root)
	}
	return nil
}

func collectCleanupFiles(root string) ([]cleanupFile, error) {
	files := make([]cleanupFile, 0)
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("slogger.cleanup.walk_entry_failed", "component", "slogger", "path", path, "err", err)
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if entry.IsDir() {
			return nil
		}
		if !isCleanupLogPath(path) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			slog.Warn("slogger.cleanup.stat_failed", "component", "slogger", "path", path, "err", infoErr)
			return fmt.Errorf("stat %s: %w", path, infoErr)
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
		return nil, fmt.Errorf("collect cleanup files under %s: %w", root, walkErr)
	}
	return files, nil
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
	return strings.Contains(base, ".jsonl") || strings.HasSuffix(base, ".log") || strings.HasSuffix(base, ".log.gz") || strings.HasSuffix(base, ".raw")
}

func cleanupGroup(path string) string {
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
