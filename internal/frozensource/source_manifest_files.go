package frozensource

import (
	"bufio"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
)

// SnapshotManifestFiles returns membership from an approved source manifest.
// VerifySnapshot must establish the actual file contents before source export.
func SnapshotManifestFiles(root, manifestPath, approvedDigest string) (files map[string]bool, err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.source_membership_failed", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	manifest, err := verifiedManifest(manifestPath, approvedDigest)
	if err != nil {
		return nil, err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve source manifest root: %w", err)
	}
	files = make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(string(manifest)))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	seen := make(map[string]bool)
	for scanner.Scan() {
		path, _, err := resolveSnapshotEntry(scanner.Text(), resolvedRoot, seen)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(resolvedRoot, path)
		if err != nil {
			return nil, fmt.Errorf("resolve source manifest membership: %w", err)
		}
		files["./"+filepath.ToSlash(relative)] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read source manifest membership: %w", err)
	}
	return files, nil
}
