package slogger

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const cleanupTailLimit = 1024 * 1024

var cleanupTimestampArchive = regexp.MustCompile(`^(.*)-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}(?:\.\d+)?(\.(?:jsonl|log))(?:\.gz)?$`)

type cleanupMutation struct {
	freed    int64
	retained int64
	changed  bool
}

// CleanupOwnsPath reports whether policy includes a file or its archive.
func CleanupOwnsPath(policy CleanupPolicy, path string) bool {
	path = filepath.Clean(path)
	group, archived := cleanupArchiveGroup(path)
	for _, owned := range policy.Paths {
		owned = filepath.Clean(owned)
		if path == owned || (archived && group == owned) {
			return true
		}
	}
	if !isCleanupLogPath(path) {
		return false
	}
	roots := append([]string(nil), policy.Roots...)
	if policy.Root != "" {
		roots = append(roots, policy.Root)
	}
	for _, root := range roots {
		relative, err := filepath.Rel(filepath.Clean(root), path)
		if err == nil && filepath.IsLocal(relative) {
			return true
		}
	}
	return false
}

// cleanupArchiveGroup recognizes lumberjack archives and legacy suffix archives.
func cleanupArchiveGroup(path string) (string, bool) {
	base := filepath.Base(path)
	if matches := cleanupTimestampArchive.FindStringSubmatch(base); len(matches) != 0 {
		return filepath.Join(filepath.Dir(path), matches[1]+matches[2]), true
	}
	for _, extension := range []string{".jsonl", ".log", ".raw"} {
		if index := strings.Index(base, extension+"."); index >= 0 {
			return filepath.Join(filepath.Dir(path), base[:index]+extension), true
		}
	}
	return path, false
}

func collectPolicyCleanupFiles(policy CleanupPolicy) (result []cleanupFile, operationError error) {
	defer func() {
		if operationError != nil {
			slog.Warn("slogger.cleanup.discovery_failed", "component", "slogger", "err", operationError)
		}
	}()
	unique := make(map[string]cleanupFile)
	var failures []error
	roots := append([]string(nil), policy.Roots...)
	if strings.TrimSpace(policy.Root) != "" {
		roots = append(roots, policy.Root)
	}
	for _, root := range roots {
		files, err := collectCleanupFiles(root)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
		for _, file := range files {
			unique[filepath.Clean(file.path)] = file
		}
	}
	for _, path := range policy.Paths {
		files, err := collectExternalCleanupFiles(path)
		if err != nil {
			failures = append(failures, err)
		}
		for _, file := range files {
			unique[file.path] = file
		}
	}
	files := make([]cleanupFile, 0, len(unique))
	for _, file := range unique {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path < files[j].path
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	return files, errors.Join(failures...)
}

func collectExternalCleanupFiles(path string) (result []cleanupFile, operationError error) {
	defer func() {
		if operationError != nil {
			slog.Warn("slogger.cleanup.external_discovery_failed", "component", "slogger", "path", path, "err", operationError)
		}
	}()
	entries, err := os.ReadDir(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list owned log %s: %w", path, err)
	}
	var files []cleanupFile
	var failures []error
	for _, entry := range entries {
		candidate := filepath.Join(filepath.Dir(path), entry.Name())
		group, archived := cleanupArchiveGroup(candidate)
		if candidate != path && (!archived || group != path) {
			continue
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			failures = append(failures, fmt.Errorf("stat owned log %s: %w", candidate, err))
			continue
		}
		if info.Mode().IsRegular() {
			files = append(files, cleanupFile{path: candidate, group: cleanupGroup(candidate), size: info.Size(), modTime: info.ModTime()})
		}
	}
	return files, errors.Join(failures...)
}

func retainCleanupFiles(files []cleanupFile, policy CleanupPolicy, result *CleanupResult) {
	archives := make([]cleanupFile, 0)
	for _, file := range files {
		result.BytesBefore += file.size
		if slices.Contains(policy.ProtectedPaths, file.path) {
			result.ProtectedBytes += file.size
		}
		if isRemovableCleanupArchive(file.path, policy) {
			archives = append(archives, file)
		}
	}
	result.BytesAfter = result.BytesBefore
	selected := make(map[string]bool)
	agePolicy := policy
	agePolicy.MaxTotalMB = 0
	for _, file := range selectCleanupCandidates(archives, agePolicy, cleanupPolicyNow()) {
		selected[file.path] = true
	}
	maxBytes := int64(policy.MaxTotalMB) * 1024 * 1024
	availableBytes := maxBytes
	if result.RetainedOpenBytes < maxBytes {
		availableBytes -= result.RetainedOpenBytes
	}
	for _, file := range archives {
		if !selected[file.path] && (maxBytes <= 0 || result.BytesAfter <= availableBytes) {
			continue
		}
		result.Candidates++
		mutation, err := removeCleanupArchive(file)
		if err != nil {
			recordCleanupFailure(result, file.path, err)
			continue
		}
		result.BytesAfter += mutation.retained - file.size
		if !mutation.changed {
			continue
		}
		result.Deleted++
		result.DeletedPaths = append(result.DeletedPaths, file.path)
		result.BytesDeleted += mutation.freed
	}
	retainActiveCleanupFiles(files, policy, maxBytes, availableBytes, result)
	if maxBytes > 0 {
		result.ExcessBytes = max(int64(0), result.BytesAfter+result.RetainedOpenBytes-maxBytes)
		if result.ExcessBytes > 0 {
			result.Errors = append(result.Errors, fmt.Sprintf("retained logs exceed the combined budget by %d bytes; protected named files account for %d bytes and %d open unlinked files retain %d bytes", result.ExcessBytes, result.ProtectedBytes, result.RetainedOpenFiles, result.RetainedOpenBytes))
		}
	}
}

func retainActiveCleanupFiles(files []cleanupFile, policy CleanupPolicy, maxBytes, availableBytes int64, result *CleanupResult) {
	for _, file := range files {
		if slices.Contains(policy.ProtectedPaths, file.path) {
			continue
		}
		if isRemovableCleanupArchive(file.path, policy) {
			continue
		}
		keep := file.size
		if policy.MaxAgeDays > 0 && file.modTime.Before(cleanupPolicyNow().Add(-time.Duration(policy.MaxAgeDays)*24*time.Hour)) {
			keep = 0
		}
		if maxBytes > 0 && result.BytesAfter > availableBytes {
			keep = min(keep, max(int64(0), file.size-(result.BytesAfter-availableBytes)))
		}
		if keep >= file.size {
			continue
		}
		result.Candidates++
		mutation, err := trimCleanupFile(file.path, file.group, keep)
		if err != nil {
			recordCleanupFailure(result, file.path, err)
			continue
		}
		result.BytesAfter += mutation.retained - file.size
		if !mutation.changed {
			continue
		}
		result.Trimmed++
		result.TrimmedPaths = append(result.TrimmedPaths, file.path)
		result.BytesTrimmed += mutation.freed
	}
}

func isRemovableCleanupArchive(path string, policy CleanupPolicy) bool {
	_, archived := cleanupArchiveGroup(path)
	return archived && !slices.Contains(policy.KeepInPlacePaths, path) && !slices.Contains(policy.ProtectedPaths, path)
}

func recordCleanupFailure(result *CleanupResult, path string, err error) {
	result.Skipped = append(result.Skipped, path)
	result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", path, err))
}

func removeCleanupArchive(candidate cleanupFile) (result cleanupMutation, operationError error) {
	defer func() {
		if operationError != nil {
			slog.Warn("slogger.cleanup.remove_failed", "component", "slogger", "path", candidate.path, "err", operationError)
		}
	}()
	lock := flock.New(candidate.group + ".lock")
	locked, err := lock.TryLock()
	if err != nil {
		return cleanupMutation{}, fmt.Errorf("lock archive %s: %w", candidate.path, err)
	}
	if !locked {
		return cleanupMutation{}, errors.New("log writer is busy")
	}
	defer func() { _ = lock.Close() }()
	info, err := os.Lstat(candidate.path)
	if errors.Is(err, os.ErrNotExist) {
		return cleanupMutation{freed: 0, retained: 0, changed: false}, nil
	}
	if err != nil {
		return cleanupMutation{}, fmt.Errorf("stat archive %s: %w", candidate.path, err)
	}
	if !info.Mode().IsRegular() {
		return cleanupMutation{}, errors.New("log archive is not a regular file")
	}
	if err := os.Remove(candidate.path); err != nil {
		return cleanupMutation{}, fmt.Errorf("remove archive %s: %w", candidate.path, err)
	}
	return cleanupMutation{freed: info.Size(), retained: 0, changed: true}, nil
}

// trimCleanupFile retains complete trailing lines on the existing inode.
// The tail read never exceeds cleanupTailLimit, even for a legacy large file.
func trimCleanupFile(path, group string, keep int64) (result cleanupMutation, operationError error) {
	defer func() {
		if operationError != nil {
			slog.Warn("slogger.cleanup.trim_failed", "component", "slogger", "path", path, "err", operationError)
		}
	}()
	lock := flock.New(group + ".lock")
	locked, err := lock.TryLock()
	if err != nil {
		return cleanupMutation{}, fmt.Errorf("lock active log %s: %w", path, err)
	}
	if !locked {
		return cleanupMutation{}, errors.New("log writer is busy")
	}
	defer func() { _ = lock.Close() }()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return cleanupMutation{freed: 0, retained: 0, changed: false}, nil
	}
	if err != nil {
		return cleanupMutation{}, fmt.Errorf("open active log %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return cleanupMutation{}, fmt.Errorf("stat active log %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return cleanupMutation{}, errors.New("active log is not a regular file")
	}
	keep = min(keep, cleanupTailLimit)
	if keep >= info.Size() {
		return cleanupMutation{freed: 0, retained: info.Size(), changed: false}, nil
	}
	start := max(int64(0), info.Size()-keep)
	tail := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return cleanupMutation{}, fmt.Errorf("read active log tail %s: %w", path, err)
	}
	if start > 0 {
		if newline := bytes.IndexByte(tail, '\n'); newline >= 0 {
			tail = tail[newline+1:]
		} else {
			tail = nil
		}
	}
	if newline := bytes.LastIndexByte(tail, '\n'); newline >= 0 {
		tail = tail[:newline+1]
	} else {
		tail = nil
	}
	if len(tail) != 0 {
		if _, err := file.WriteAt(tail, 0); err != nil {
			return cleanupMutation{}, fmt.Errorf("rewrite active log tail %s: %w", path, err)
		}
	}
	if err := file.Truncate(int64(len(tail))); err != nil {
		return cleanupMutation{}, fmt.Errorf("truncate active log %s: %w", path, err)
	}
	return cleanupMutation{freed: info.Size() - int64(len(tail)), retained: int64(len(tail)), changed: true}, nil
}
