package cursorstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// ForEachStoredComposerBubble visits each stored bubble for a composer.
func ForEachStoredComposerBubble(ctx context.Context, db *sql.DB, composerID string, visit func(Bubble) error) error {
	snapshot, err := beginReadSnapshot(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.stored_bubbles_snapshot_failed", "concern", concern, "composer_id", composerID, "err", err)
		return fmt.Errorf("read stored bubbles for composer %s: %w", composerID, err)
	}
	defer snapshot.rollback()
	bounds := keyRangeForPrefix(bubbleKey(composerID, ""))
	err = forEachComposerBubbleProjection(ctx, snapshot, bounds, func(row bubbleProjection) error {
		bubble := row.bubble()
		if bubbleID, ok := bubbleIDFromKey(row.Key, composerID); ok {
			bubble.BubbleID = bubbleID
		}
		return visit(bubble)
	})
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.stored_bubbles_failed", "concern", concern, "composer_id", composerID, "err", err)
		return fmt.Errorf("read stored bubbles for composer %s: %w", composerID, err)
	}
	return nil
}
