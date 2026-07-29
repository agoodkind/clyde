package cursorstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"hash"
	"log/slog"
	"strconv"
)

// ComposerBubbleStock summarises what one chat holds in its own bubble key range,
// for a caller deciding whether the chat is a conversation at all.
//
// Discovery needs this because a chat's header reference list is not a complete
// index of its bubbles, so an empty list is not evidence of an empty chat.
// Measured on a real store, 631 of 2,470 chats list no bubble references, 9 of
// them hold stored bubbles anyway, and those 9 hold 2,245 content-bearing rows
// between them, the largest a 2,189-message agent run.
type ComposerBubbleStock struct {
	// StoredRows is how many bubble rows the chat's key range holds.
	StoredRows int64
	// Revision identifies the projected content and ordering fields of the whole
	// key range. A replacement changes it even when the row count and composer
	// header do not, so the conversation index re-reads changed text.
	Revision int64
	// HasContent reports whether at least one of them carries something a reader
	// would see, which is the test that separates a real chat from the 622 empty
	// drafts and panes in that same sample.
	HasContent bool
	// Conclusive is false when the read stopped before it could show the chat
	// holds nothing. A chat every row of which went unread looks exactly like an
	// empty draft from here, and the two must not share an answer: the caller
	// decides whether a chat is a conversation at all, and a discovery pass that
	// drops a chat does not merely skip it, it takes the chat's existing record
	// out of the index with it.
	Conclusive bool
}

// ReadComposerBubbleStock counts one chat's stored bubble rows, reports whether
// any carries content, and derives a revision from every projected row.
//
// A negative answer is only conclusive when every stored row was read. Finding
// content is conclusive whatever went unread, but the scan still visits every
// projected row because the revision must change when any stored content changes.
func ReadComposerBubbleStock(
	ctx context.Context,
	db *sql.DB,
	composerID string,
) (ComposerBubbleStock, error) {
	stock := ComposerBubbleStock{StoredRows: 0, Revision: 0, HasContent: false, Conclusive: false}
	bounds := keyRangeForPrefix(bubbleKeyPrefix + composerID + ":")

	// One snapshot for both statements, so a row Cursor appends between them
	// cannot make the projection reach the count while a row it could not read
	// goes unnoticed.
	snapshot, err := beginReadSnapshot(ctx, db)
	if err != nil {
		return stock, err
	}
	defer snapshot.rollback()

	storedRows, err := snapshot.countRange(ctx, bounds)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_bubble_count_failed", "concern", concern, "composer_id", composerID, "err", err)
		return stock, fmt.Errorf("count cursor bubbles for composer %q: %w", composerID, err)
	}
	stock.StoredRows = int64(storedRows)
	if storedRows == 0 {
		stock.Conclusive = true
		return stock, nil
	}

	revisionHasher := sha256.New()
	writeFingerprintField(revisionHasher, strconv.Itoa(storedRows))
	projected := 0
	err = forEachComposerBubbleProjection(ctx, snapshot, bounds, func(row bubbleProjection) error {
		projected++
		bubble := row.bubble()
		digest := newBubbleDigest(row.Key, bubble, row.RowID)
		writeFingerprintField(revisionHasher, row.Key)
		writeFingerprintField(revisionHasher, strconv.FormatInt(row.RowID, 10))
		writeFingerprintField(revisionHasher, digest.ServerBubbleID)
		writeFingerprintField(revisionHasher, bubble.CreatedAt)
		writeFingerprintField(revisionHasher, string(digest.Fingerprint[:]))
		if bubble.HasContent() {
			stock.HasContent = true
		}
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_bubble_stock_failed", "concern", concern, "composer_id", composerID, "err", err)
		return stock, fmt.Errorf("read cursor bubble stock for composer %q: %w", composerID, err)
	}
	stock.Revision = revisionFromHasher(revisionHasher)
	stock.Conclusive = stock.HasContent || projected >= storedRows
	if !stock.Conclusive {
		slog.WarnContext(ctx, "providers.cursor.store.composer_bubble_stock_inconclusive", "concern", concern, "composer_id", composerID, "stored", storedRows, "projected", projected)
	}
	return stock, nil
}

func revisionFromHasher(hasher hash.Hash) int64 {
	sum := hasher.Sum(nil)
	revision := int64(binary.BigEndian.Uint64(sum[:8]) & ((uint64(1) << 63) - 1))
	if revision == 0 {
		return 1
	}
	return revision
}
