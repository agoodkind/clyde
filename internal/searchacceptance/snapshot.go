package searchacceptance

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/frozensource"
)

// SnapshotVerification records files verified against a frozen source manifest.
type SnapshotVerification = frozensource.SnapshotVerification

// VerifySnapshot hashes every manifest file without retaining source contents.
func VerifySnapshot(ctx context.Context, root, manifestPath, expectedDigest string) (SnapshotVerification, error) {
	verification, err := frozensource.VerifySnapshot(ctx, root, manifestPath, expectedDigest)
	if err != nil {
		slog.WarnContext(ctx, "frozen snapshot verification failed", "err", err)
		return verification, fmt.Errorf("verify frozen snapshot: %w", err)
	}
	return verification, nil
}
