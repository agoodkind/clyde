package searchacceptance

import (
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/frozensource"
)

// SnapshotManifestFiles returns approved manifest membership. VerifySnapshot
// must establish the actual contents before source reads.
func SnapshotManifestFiles(root, manifestPath, approvedDigest string) (map[string]bool, error) {
	files, err := frozensource.SnapshotManifestFiles(root, manifestPath, approvedDigest)
	if err != nil {
		slog.Warn("frozen manifest membership failed", "err", err)
		return nil, fmt.Errorf("read frozen manifest membership: %w", err)
	}
	return files, nil
}
