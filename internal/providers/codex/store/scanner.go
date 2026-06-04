package codexstore

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DiscoveryScanner is part of Clyde's typed adapter surface.
type DiscoveryScanner struct {
	Paths StorePaths
}

// NewDiscoveryScanner is part of Clyde's typed adapter surface.
func NewDiscoveryScanner(paths StorePaths) DiscoveryScanner {
	return DiscoveryScanner{Paths: paths}
}

// RolloutStamp is the file size and modification time used to detect whether a
// rollout file changed since the last incremental scan. Rollout files are
// append-only, so a matching size and mtime means the parsed contents are
// unchanged and can be skipped.
type RolloutStamp struct {
	Size  int64
	Mtime time.Time
}

// RolloutCandidate is one rollout file surfaced by discovery, with the file
// stamp used to skip unchanged files and the archived flag that records which
// root it came from.
type RolloutCandidate struct {
	Path     string
	Stamp    RolloutStamp
	Archived bool
}

// DiscoverCandidates walks every rollout root and returns one candidate per
// rollout file without opening or parsing it. The caller stats nothing further
// for unchanged files and reads only changed files through
// [ReadThreadByRolloutPath].
func (s DiscoveryScanner) DiscoverCandidates() ([]RolloutCandidate, error) {
	var out []RolloutCandidate
	for _, root := range rolloutRoots(s.Paths) {
		err := filepath.WalkDir(root.Path, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
					return nil
				}
				return err
			}
			if d.IsDir() || !isRolloutFilename(d.Name()) {
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				if errors.Is(infoErr, os.ErrNotExist) {
					return nil
				}
				slog.Warn("codex.store.scanner.stat_failed", "concern", "providers.codex.store", "path", path, "err", infoErr)
				return nil
			}
			out = append(out, RolloutCandidate{
				Path:     path,
				Stamp:    RolloutStamp{Size: info.Size(), Mtime: info.ModTime()},
				Archived: root.Archived,
			})
			return nil
		})
		if err != nil {
			slog.Warn("codex.store.scanner.walk_failed", "concern", "providers.codex.store", "root", root.Path, "err", err)
			return nil, fmt.Errorf("walk codex rollout tree: %w", err)
		}
	}
	return out, nil
}

func isRolloutFilename(name string) bool {
	return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
}

// RolloutIdentityFromFilename derives the thread id and created time encoded in
// a rollout filename, used as the fallback identity when a rollout file is too
// short or malformed to parse a header. The boolean is false when name is not a
// recognizable rollout filename.
func RolloutIdentityFromFilename(name string) (string, time.Time, bool) {
	return rolloutIdentityFromFilename(name)
}

func rolloutIdentityFromFilename(name string) (string, time.Time, bool) {
	if !isRolloutFilename(name) {
		return "", time.Time{}, false
	}
	base := strings.TrimSuffix(strings.TrimPrefix(name, "rollout-"), ".jsonl")
	if len(base) < len("2006-01-02T15-04-05-")+1 {
		return "", time.Time{}, false
	}
	timestampPart := base[:len("2006-01-02T15-04-05")]
	if len(base) <= len(timestampPart)+1 {
		return "", time.Time{}, false
	}
	threadID := base[len(timestampPart)+1:]
	createdAt, err := time.ParseInLocation("2006-01-02T15-04-05", timestampPart, time.UTC)
	if err != nil || strings.TrimSpace(threadID) == "" {
		return "", time.Time{}, false
	}
	return threadID, createdAt, true
}
