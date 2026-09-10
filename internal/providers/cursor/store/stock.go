package cursorstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"hash"
	"strconv"
)

type composerStockBuilder struct {
	stock     ComposerBubbleStock
	projected int64
	hasher    hash.Hash
}

// ReadComposerBubbleStocks projects the global bubble range once. Counts and
// projections share a snapshot, including bubbles absent from header references.
// A missing map entry means the composer has no stored bubble rows.
func ReadComposerBubbleStocks(ctx context.Context, db *sql.DB) (map[string]ComposerBubbleStock, error) {
	snapshot, err := beginReadSnapshot(ctx, db)
	if err != nil {
		return nil, err
	}
	defer snapshot.rollback()
	stocks := make(map[string]ComposerBubbleStock)
	exists, err := snapshot.tableExists(ctx, string(KVTableCursorDiskKV))
	if err != nil || !exists {
		return stocks, err
	}
	bounds := keyRangeForPrefix(bubbleKeyPrefix)
	rows, err := snapshot.queryRange(ctx, "SELECT key FROM cursorDiskKV"+keyRangePredicate(bounds, ""), bounds, "bubble stock keys")
	if err != nil {
		logger := discoveryReadLogger(ctx)
		logger.WarnContext(ctx, "providers.cursor.store.composer_bubble_count_failed", "concern", concern, "err", err)
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	builders := make(map[string]*composerStockBuilder)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan cursor bubble stock key: %w", err)
		}
		id, _, ok := parseBubbleKey(key)
		if !ok {
			continue
		}
		if builders[id] == nil {
			builders[id] = &composerStockBuilder{stock: ComposerBubbleStock{StoredRows: 0, Revision: 0, HasContent: false, Conclusive: false}, projected: 0, hasher: sha256.New()}
		}
		builders[id].stock.StoredRows++
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("iterate cursor bubble stock keys: %w", err)
	}
	for _, builder := range builders {
		writeFingerprintField(builder.hasher, strconv.FormatInt(builder.stock.StoredRows, 10))
	}
	err = forEachComposerBubbleProjection(ctx, snapshot, bounds, func(row bubbleProjection) error {
		id, _, ok := parseBubbleKey(row.Key)
		if !ok {
			return nil
		}
		builder := builders[id]
		builder.projected++
		bubble := row.bubble()
		digest := newBubbleDigest(row.Key, bubble, row.RowID)
		writeStockFingerprint(builder.hasher, row, digest, bubble)
		builder.stock.HasContent = builder.stock.HasContent || bubble.HasContent()
		return nil
	})
	if err != nil {
		return nil, err
	}
	for id, builder := range builders {
		builder.stock.Revision = revisionFromHasher(builder.hasher)
		builder.stock.Conclusive = builder.stock.HasContent || builder.projected >= builder.stock.StoredRows
		stocks[id] = builder.stock
	}
	return stocks, nil
}

func writeStockFingerprint(hasher hash.Hash, row bubbleProjection, digest bubbleDigest, bubble Bubble) {
	writeFingerprintField(hasher, row.Key)
	writeFingerprintField(hasher, strconv.FormatInt(row.RowID, 10))
	writeFingerprintField(hasher, digest.ServerBubbleID)
	writeFingerprintField(hasher, bubble.CreatedAt)
	writeFingerprintField(hasher, string(digest.Fingerprint[:]))
}

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

func revisionFromHasher(hasher hash.Hash) int64 {
	sum := hasher.Sum(nil)
	revision := int64(binary.BigEndian.Uint64(sum[:8]) & ((uint64(1) << 63) - 1))
	if revision == 0 {
		return 1
	}
	return revision
}
