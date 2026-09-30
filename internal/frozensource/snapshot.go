package frozensource

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// SnapshotVerification records files verified against a frozen source manifest.
type SnapshotVerification struct {
	ManifestDigest string `json:"manifest_sha256"`
	Files          int    `json:"files"`
	Bytes          int64  `json:"bytes"`
}

// VerifySnapshot hashes every manifest file without retaining source contents.
func VerifySnapshot(ctx context.Context, root, manifestPath, expectedDigest string) (result SnapshotVerification, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "search.acceptance.snapshot_rejected", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	if !filepath.IsAbs(root) || !filepath.IsAbs(manifestPath) {
		return result, errors.New("snapshot and manifest paths must be absolute")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return result, fmt.Errorf("resolve snapshot root: %w", err)
	}
	manifest, err := verifiedManifest(manifestPath, expectedDigest)
	if err != nil {
		return result, err
	}
	result.ManifestDigest = strings.ToLower(expectedDigest)
	scanner := bufio.NewScanner(strings.NewReader(string(manifest)))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	seen := make(map[string]bool)
	buffer := make([]byte, 256*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("verify frozen snapshot: %w", err)
		}
		path, fileDigest, err := resolveSnapshotEntry(scanner.Text(), resolvedRoot, seen)
		if err != nil {
			return result, err
		}
		count, err := verifySnapshotFile(ctx, path, fileDigest, buffer)
		if err != nil {
			return result, fmt.Errorf("verify snapshot entry %q: %w", path, err)
		}
		result.Files++
		result.Bytes += count
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read snapshot entries: %w", err)
	}
	if result.Files == 0 {
		return result, errors.New("frozen manifest contains no files")
	}
	return result, nil
}

func verifiedManifest(path, expectedDigest string) (content []byte, err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.manifest_rejected", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	if len(expectedDigest) != sha256.Size*2 {
		return nil, errors.New("frozen manifest requires a SHA-256 digest")
	}
	if _, err := hex.DecodeString(expectedDigest); err != nil {
		return nil, fmt.Errorf("decode frozen manifest digest: %w", err)
	}
	manifest, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read frozen manifest: %w", err)
	}
	digest := sha256.Sum256(manifest)
	if hex.EncodeToString(digest[:]) != strings.ToLower(expectedDigest) {
		return nil, errors.New("frozen manifest digest differs from the approved digest")
	}
	return manifest, nil
}

func resolveSnapshotEntry(line, root string, seen map[string]bool) (resolvedPath, fileDigest string, err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.source_path_rejected", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	if len(line) < sha256.Size*2+3 || line[sha256.Size*2:sha256.Size*2+2] != "  " {
		return "", "", errors.New("manifest entry has invalid SHA-256 syntax")
	}
	digest := line[:sha256.Size*2]
	if _, err := hex.DecodeString(digest); err != nil {
		return "", "", fmt.Errorf("decode manifest entry digest: %w", err)
	}
	name := line[sha256.Size*2+2:]
	clean := filepath.Clean(name)
	if filepath.IsAbs(name) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || seen[clean] {
		return "", "", fmt.Errorf("manifest entry has an invalid or repeated relative path: %q", name)
	}
	seen[clean] = true
	path, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", "", fmt.Errorf("resolve snapshot entry %q: %w", name, err)
	}
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("snapshot entry resolves outside the snapshot: %q", name)
	}
	return path, strings.ToLower(digest), nil
}

func verifySnapshotFile(ctx context.Context, path, expectedDigest string, buffer []byte) (count int64, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "search.acceptance.source_rejected", "component", "searchacceptance", "concern", "source", "err", err)
		}
	}()
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open frozen source: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("inspect frozen source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("snapshot entry is not a regular file")
	}
	hash := sha256.New()
	for {
		if err := ctx.Err(); err != nil {
			return count, fmt.Errorf("hash frozen source: %w", err)
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
			count += int64(read)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return count, fmt.Errorf("read frozen source: %w", readErr)
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return count, errors.New("source file differs from the frozen manifest")
	}
	return count, nil
}
