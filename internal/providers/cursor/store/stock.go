package cursorstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"maps"
	"sort"
	"strconv"
	"strings"
)

type bubbleRefreshPath string

const (
	bubbleRefreshPathDelta         bubbleRefreshPath = "delta"
	bubbleRefreshPathActiveInPlace bubbleRefreshPath = "active_in_place"
	bubbleRefreshPathFull          bubbleRefreshPath = "full_reconciliation"
	changedComposerRowsQuery                         = "SELECT rowid, key FROM cursorDiskKV NOT INDEXED WHERE rowid > ? ORDER BY rowid"
)

type bubbleChangeState struct {
	rows    int64
	lastRow int64
	lastKey string
}

type composerStockBuilder struct {
	stock     ComposerBubbleStock
	projected int64
	hasher    hash.Hash
}

type composerBubbleSelector struct {
	digest     [sha256.Size]byte
	storedRows int64
}

type composerSelectorBuilder struct {
	selector composerBubbleSelector
	hasher   hash.Hash
}

type bubbleSelectorInventory struct {
	selectors map[string]composerBubbleSelector
	digest    [sha256.Size]byte
	rows      int64
	lastRow   int64
	bytes     int64
}

type composerStockRefreshStats struct {
	totalComposers     int
	reusedComposers    int
	projectedComposers int
	projectedRows      int64
	projectionQueries  int
	selectorQueries    int
}

func readBubbleChangeState(ctx context.Context, snapshot readSnapshot) (bubbleChangeState, error) {
	var state bubbleChangeState
	logger := discoveryReadLogger(ctx)
	exists, err := snapshot.tableExists(ctx, string(KVTableCursorDiskKV))
	if err != nil {
		logger.WarnContext(ctx, "providers.cursor.store.bubble_change_table_failed", "concern", concern, "err", err)
		return state, err
	}
	if !exists {
		return state, nil
	}
	bounds := keyRangeForPrefix(bubbleKeyPrefix)
	countQuery := "SELECT count(*) FROM cursorDiskKV" + keyRangePredicate(bounds, "")
	if err := snapshot.tx.QueryRowContext(ctx, countQuery, bounds.Lower, bounds.Upper).Scan(&state.rows); err != nil {
		logger.WarnContext(ctx, "providers.cursor.store.bubble_change_count_failed", "concern", concern, "err", err)
		return state, fmt.Errorf("count cursor bubble change rows: %w", err)
	}
	err = snapshot.tx.QueryRowContext(ctx,
		"SELECT rowid, key FROM cursorDiskKV NOT INDEXED ORDER BY rowid DESC LIMIT 1",
	).Scan(&state.lastRow, &state.lastKey)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		logger.WarnContext(ctx, "providers.cursor.store.change_high_water_failed", "concern", concern, "err", err)
		return state, fmt.Errorf("read cursor change high water row: %w", err)
	}
	return state, nil
}

func readChangedComposerIDs(
	ctx context.Context,
	snapshot readSnapshot,
	afterRow int64,
) (map[string]bool, int64, error) {
	logger := discoveryReadLogger(ctx)
	rows, err := snapshot.tx.QueryContext(ctx, changedComposerRowsQuery, afterRow)
	if err != nil {
		logger.WarnContext(ctx, "providers.cursor.store.changed_rows_query_failed", "concern", concern, "after_row", afterRow, "err", err)
		return nil, 0, fmt.Errorf("query cursor changed rows after %d: %w", afterRow, err)
	}
	defer func() { _ = rows.Close() }()
	changed := make(map[string]bool)
	var newRows int64
	for rows.Next() {
		var rowID int64
		var key string
		if err := rows.Scan(&rowID, &key); err != nil {
			logger.WarnContext(ctx, "providers.cursor.store.changed_row_scan_failed", "concern", concern, "err", err)
			return nil, 0, fmt.Errorf("scan cursor changed row: %w", err)
		}
		newRows++
		if composerID, _, ok := parseBubbleKey(key); ok {
			changed[composerID] = true
			continue
		}
		if composerID, ok := strings.CutPrefix(key, composerDataKeyPrefix); ok && composerID != "" {
			changed[composerID] = true
		}
	}
	if err := rows.Err(); err != nil {
		logger.WarnContext(ctx, "providers.cursor.store.changed_rows_iterate_failed", "concern", concern, "err", err)
		return nil, 0, fmt.Errorf("iterate cursor changed rows: %w", err)
	}
	return changed, newRows, nil
}

func readMostRecentlyActiveComposerID(ctx context.Context, snapshot readSnapshot) (string, int, error) {
	logger := discoveryReadLogger(ctx)
	exists, err := snapshot.tableExists(ctx, composerHeadersTable)
	if err != nil {
		logger.WarnContext(ctx, "providers.cursor.store.active_composer_table_failed", "concern", concern, "err", err)
		return "", 1, err
	}
	if !exists {
		return "", 1, nil
	}
	var composerID string
	err = snapshot.tx.QueryRowContext(ctx,
		"SELECT composerId FROM composerHeaders ORDER BY CAST(lastUpdatedAt AS INTEGER) DESC, rowid DESC LIMIT 1",
	).Scan(&composerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 2, nil
	}
	if err != nil {
		logger.WarnContext(ctx, "providers.cursor.store.active_composer_query_failed", "concern", concern, "err", err)
		return "", 2, fmt.Errorf("read cursor active composer: %w", err)
	}
	return composerID, 2, nil
}

func expectedBubbleRows(
	priorRows int64,
	changed map[string]bool,
	prior map[string]composerBubbleSelector,
	current map[string]composerBubbleSelector,
) int64 {
	expected := priorRows
	for composerID := range changed {
		expected -= prior[composerID].storedRows
		expected += current[composerID].storedRows
	}
	return expected
}

func refreshChangedComposerBubbleSelectors(
	ctx context.Context,
	snapshot readSnapshot,
	changed map[string]bool,
	prior map[string]composerBubbleSelector,
	queryCount int,
) (map[string]composerBubbleSelector, int, error) {
	selectors := make(map[string]composerBubbleSelector, len(prior))
	maps.Copy(selectors, prior)
	composerIDs := make([]string, 0, len(changed))
	for composerID := range changed {
		composerIDs = append(composerIDs, composerID)
	}
	sort.Strings(composerIDs)
	for _, composerID := range composerIDs {
		selector, found, err := readComposerBubbleSelector(ctx, snapshot, composerID)
		queryCount++
		if err != nil {
			return nil, queryCount, err
		}
		if !found {
			delete(selectors, composerID)
			continue
		}
		selectors[composerID] = selector
	}
	return selectors, queryCount, nil
}

func readComposerBubbleSelector(
	ctx context.Context,
	snapshot readSnapshot,
	composerID string,
) (composerBubbleSelector, bool, error) {
	var selector composerBubbleSelector
	logger := discoveryReadLogger(ctx)
	bounds := keyRangeForPrefix(bubbleKeyPrefix + composerID + ":")
	rows, err := snapshot.queryRange(ctx,
		"SELECT key, rowid, COALESCE(octet_length(value), -1) FROM cursorDiskKV"+
			keyRangePredicate(bounds, "")+" ORDER BY key",
		bounds,
		"composer bubble stock selector",
	)
	if err != nil {
		return selector, false, err
	}
	defer func() { _ = rows.Close() }()
	hasher := sha256.New()
	for rows.Next() {
		var key string
		var rowID int64
		var valueLength int64
		if err := rows.Scan(&key, &rowID, &valueLength); err != nil {
			logger.WarnContext(ctx, "providers.cursor.store.composer_bubble_selector_scan_failed", "concern", concern, "composer_id", composerID, "err", err)
			return selector, false, fmt.Errorf("scan cursor composer bubble selector: %w", err)
		}
		selector.storedRows++
		writeFingerprintField(hasher, key)
		writeFingerprintField(hasher, strconv.FormatInt(rowID, 10))
		writeFingerprintField(hasher, strconv.FormatInt(valueLength, 10))
	}
	if err := rows.Err(); err != nil {
		logger.WarnContext(ctx, "providers.cursor.store.composer_bubble_selector_iterate_failed", "concern", concern, "composer_id", composerID, "err", err)
		return selector, false, fmt.Errorf("iterate cursor composer bubble selector: %w", err)
	}
	if selector.storedRows == 0 {
		return selector, false, nil
	}
	copy(selector.digest[:], hasher.Sum(nil))
	return selector, true, nil
}

func readComposerBubbleSelectors(ctx context.Context, snapshot readSnapshot) (bubbleSelectorInventory, error) {
	inventory := bubbleSelectorInventory{
		selectors: make(map[string]composerBubbleSelector),
		digest:    [sha256.Size]byte{},
		rows:      0,
		lastRow:   0,
		bytes:     0,
	}
	exists, err := snapshot.tableExists(ctx, string(KVTableCursorDiskKV))
	if err != nil || !exists {
		return inventory, err
	}
	bounds := keyRangeForPrefix(bubbleKeyPrefix)
	rows, err := snapshot.queryRange(ctx,
		"SELECT key, rowid, COALESCE(octet_length(value), -1) FROM cursorDiskKV"+
			keyRangePredicate(bounds, "")+" ORDER BY key",
		bounds,
		"bubble stock selectors",
	)
	if err != nil {
		logger := discoveryReadLogger(ctx)
		logger.WarnContext(ctx, "providers.cursor.store.composer_bubble_count_failed", "concern", concern, "err", err)
		return inventory, err
	}
	defer func() { _ = rows.Close() }()
	builders := make(map[string]*composerSelectorBuilder)
	globalHasher := sha256.New()
	for rows.Next() {
		var key string
		var rowID, valueLength int64
		if err := rows.Scan(&key, &rowID, &valueLength); err != nil {
			return inventory, fmt.Errorf("scan cursor bubble stock selector: %w", err)
		}
		inventory.rows++
		inventory.lastRow = max(inventory.lastRow, rowID)
		if valueLength >= 0 {
			inventory.bytes += valueLength
		}
		writeFingerprintField(globalHasher, key)
		writeFingerprintField(globalHasher, strconv.FormatInt(rowID, 10))
		writeFingerprintField(globalHasher, strconv.FormatInt(valueLength, 10))
		id, _, ok := parseBubbleKey(key)
		if !ok {
			continue
		}
		if builders[id] == nil {
			builders[id] = &composerSelectorBuilder{
				selector: composerBubbleSelector{digest: [sha256.Size]byte{}, storedRows: 0},
				hasher:   sha256.New(),
			}
		}
		builder := builders[id]
		builder.selector.storedRows++
		writeFingerprintField(builder.hasher, key)
		writeFingerprintField(builder.hasher, strconv.FormatInt(rowID, 10))
		writeFingerprintField(builder.hasher, strconv.FormatInt(valueLength, 10))
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return inventory, fmt.Errorf("iterate cursor bubble stock selectors: %w", err)
	}
	copy(inventory.digest[:], globalHasher.Sum(nil))
	for id, builder := range builders {
		copy(builder.selector.digest[:], builder.hasher.Sum(nil))
		inventory.selectors[id] = builder.selector
	}
	return inventory, nil
}

func refreshComposerBubbleStocksInSnapshot(
	ctx context.Context,
	snapshot readSnapshot,
	selectors map[string]composerBubbleSelector,
	priorStocks map[string]ComposerBubbleStock,
	priorSelectors map[string]composerBubbleSelector,
) (map[string]ComposerBubbleStock, map[string]composerBubbleSelector, composerStockRefreshStats, error) {
	stocks := make(map[string]ComposerBubbleStock)
	stats := composerStockRefreshStats{
		totalComposers:     len(selectors),
		reusedComposers:    0,
		projectedComposers: 0,
		projectedRows:      0,
		projectionQueries:  0,
		selectorQueries:    0,
	}
	composerIDs := make([]string, 0, len(selectors))
	for composerID := range selectors {
		composerIDs = append(composerIDs, composerID)
	}
	sort.Strings(composerIDs)
	if len(priorStocks) == 0 && len(priorSelectors) == 0 {
		if len(composerIDs) == 0 {
			return stocks, selectors, stats, nil
		}
		var err error
		var projectedRows int64
		stocks, projectedRows, err = projectAllComposerBubbleStocks(ctx, snapshot, keyRangeForPrefix(bubbleKeyPrefix), selectors, composerIDs)
		if err != nil {
			return nil, nil, composerStockRefreshStats{}, err
		}
		stats.projectedComposers = len(composerIDs)
		stats.projectedRows = projectedRows
		stats.projectionQueries = 1
		return stocks, selectors, stats, nil
	}
	for _, id := range composerIDs {
		selector := selectors[id]
		priorSelector, hadSelector := priorSelectors[id]
		priorStock, hadStock := priorStocks[id]
		if hadSelector && priorSelector == selector && hadStock {
			stocks[id] = priorStock
			stats.reusedComposers++
			continue
		}
		stock, projected, err := projectComposerBubbleStock(ctx, snapshot, id, selector.storedRows)
		if err != nil {
			return nil, nil, composerStockRefreshStats{}, err
		}
		stocks[id] = stock
		stats.projectedComposers++
		stats.projectedRows += projected
		stats.projectionQueries++
	}
	return stocks, selectors, stats, nil
}

func refreshChangedComposerBubbleStocks(
	ctx context.Context,
	snapshot readSnapshot,
	changed map[string]bool,
	selectors map[string]composerBubbleSelector,
	priorStocks map[string]ComposerBubbleStock,
) (map[string]ComposerBubbleStock, composerStockRefreshStats, error) {
	stocks := make(map[string]ComposerBubbleStock, len(priorStocks))
	maps.Copy(stocks, priorStocks)
	stats := composerStockRefreshStats{
		totalComposers:     len(selectors),
		reusedComposers:    0,
		projectedComposers: 0,
		projectedRows:      0,
		projectionQueries:  0,
		selectorQueries:    0,
	}
	composerIDs := make([]string, 0, len(changed))
	for composerID := range changed {
		composerIDs = append(composerIDs, composerID)
	}
	sort.Strings(composerIDs)
	for _, composerID := range composerIDs {
		selector, found := selectors[composerID]
		if !found {
			delete(stocks, composerID)
			continue
		}
		stock, projected, err := projectComposerBubbleStock(ctx, snapshot, composerID, selector.storedRows)
		if err != nil {
			return nil, composerStockRefreshStats{}, err
		}
		stocks[composerID] = stock
		stats.projectedComposers++
		stats.projectedRows += projected
		stats.projectionQueries++
	}
	stats.reusedComposers = stats.totalComposers - stats.projectedComposers
	return stocks, stats, nil
}

func projectAllComposerBubbleStocks(
	ctx context.Context,
	snapshot readSnapshot,
	bounds keyRange,
	selectors map[string]composerBubbleSelector,
	composerIDs []string,
) (map[string]ComposerBubbleStock, int64, error) {
	builders := make(map[string]*composerStockBuilder, len(composerIDs))
	for _, composerID := range composerIDs {
		storedRows := selectors[composerID].storedRows
		builder := &composerStockBuilder{
			stock: ComposerBubbleStock{
				StoredRows: storedRows,
				Revision:   0,
				HasContent: false,
				Conclusive: false,
			},
			projected: 0,
			hasher:    sha256.New(),
		}
		writeFingerprintField(builder.hasher, strconv.FormatInt(storedRows, 10))
		builders[composerID] = builder
	}
	err := forEachComposerBubbleProjection(ctx, snapshot, bounds, func(row bubbleProjection) error {
		composerID, _, ok := parseBubbleKey(row.Key)
		if !ok {
			return nil
		}
		builder := builders[composerID]
		builder.projected++
		bubble := row.bubble()
		digest := newBubbleDigest(row.Key, bubble, row.RowID)
		writeStockFingerprint(builder.hasher, row, digest, bubble)
		builder.stock.HasContent = builder.stock.HasContent || bubble.HasContent()
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	stocks := make(map[string]ComposerBubbleStock, len(builders))
	var projectedRows int64
	for _, composerID := range composerIDs {
		builder := builders[composerID]
		builder.stock.Revision = revisionFromHasher(builder.hasher)
		builder.stock.Conclusive = builder.stock.HasContent || builder.projected >= builder.stock.StoredRows
		stocks[composerID] = builder.stock
		projectedRows += builder.projected
	}
	return stocks, projectedRows, nil
}

func projectComposerBubbleStock(
	ctx context.Context,
	snapshot readSnapshot,
	composerID string,
	storedRows int64,
) (ComposerBubbleStock, int64, error) {
	builder := composerStockBuilder{
		stock: ComposerBubbleStock{
			StoredRows: storedRows,
			Revision:   0,
			HasContent: false,
			Conclusive: false,
		},
		projected: 0,
		hasher:    sha256.New(),
	}
	writeFingerprintField(builder.hasher, strconv.FormatInt(storedRows, 10))
	bounds := keyRangeForPrefix(bubbleKeyPrefix + composerID + ":")
	err := forEachComposerBubbleProjection(ctx, snapshot, bounds, func(row bubbleProjection) error {
		builder.projected++
		bubble := row.bubble()
		digest := newBubbleDigest(row.Key, bubble, row.RowID)
		writeStockFingerprint(builder.hasher, row, digest, bubble)
		builder.stock.HasContent = builder.stock.HasContent || bubble.HasContent()
		return nil
	})
	if err != nil {
		return ComposerBubbleStock{}, 0, err
	}
	builder.stock.Revision = revisionFromHasher(builder.hasher)
	builder.stock.Conclusive = builder.stock.HasContent || builder.projected >= storedRows
	return builder.stock, builder.projected, nil
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
