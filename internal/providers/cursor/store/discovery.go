package cursorstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"goodkind.io/clyde/internal/clock"
)

type fileMetadata struct {
	size    int64
	mtime   time.Time
	mode    os.FileMode
	absent  bool
	failure string
}

func readFileMetadata(path string) fileMetadata {
	if path == "" {
		return fileMetadata{size: 0, mtime: time.Time{}, mode: 0, absent: true, failure: ""}
	}
	info, err := os.Stat(path)
	if err != nil {
		return fileMetadata{size: 0, mtime: time.Time{}, mode: 0, absent: StatSaysAbsent(err, path), failure: err.Error()}
	}
	return fileMetadata{size: info.Size(), mtime: info.ModTime(), mode: info.Mode(), absent: false, failure: ""}
}

type databaseMetadata struct {
	database fileMetadata
	wal      fileMetadata
}

func readDatabaseMetadata(path string) databaseMetadata {
	return databaseMetadata{database: readFileMetadata(path), wal: readFileMetadata(path + "-wal")}
}

// WorkspaceDiscovery holds the decoded discovery inputs from one workspace.
// Failed components retain their prior contribution and carry their read error.
type WorkspaceDiscovery struct {
	Registry       AllComposersDocument
	RegistryErr    error
	Legacy         LegacyChatData
	LegacyErr      error
	Generations    []GenerationEntry
	GenerationsErr error
	WorkspaceRoot  string
	DescriptorErr  error
	Revision       int64
	Mtime          time.Time
}

type workspaceCacheEntry struct {
	completed   atomic.Uint64
	diagnostics cachedReadDiagnostics
	mu          sync.Mutex
	known       bool
	stamp       databaseMetadata
	data        WorkspaceDiscovery
}

type descriptorCacheEntry struct {
	completed atomic.Uint64
	mu        sync.Mutex
	known     bool
	stamp     fileMetadata
	path      string
	err       error
}

type discoveryCache struct {
	mu          sync.Mutex
	workspaces  map[string]*workspaceCacheEntry
	globals     map[string]*globalCacheEntry
	descriptors map[string]*descriptorCacheEntry
	listings    map[string]WorkspaceListing
}

var sharedDiscovery = discoveryCache{
	mu:          sync.Mutex{},
	workspaces:  make(map[string]*workspaceCacheEntry),
	globals:     make(map[string]*globalCacheEntry),
	descriptors: make(map[string]*descriptorCacheEntry),
	listings:    make(map[string]WorkspaceListing),
}

// ReadWorkspaceDiscovery shares one changed-store read across composer metadata,
// legacy chat discovery, and request-ring lookup. No connection survives the read.
func ReadWorkspaceDiscovery(ctx context.Context, entry WorkspaceEntry) WorkspaceDiscovery {
	cached := sharedDiscovery.workspaceEntry(entry.StateDBPath)
	completed := cached.completed.Load()
	cached.mu.Lock()
	defer cached.mu.Unlock()
	stamp := readDatabaseMetadata(entry.StateDBPath)
	data := cached.data
	changed := !cached.known || cached.stamp != stamp
	availability := isReadAvailabilityError(errors.Join(data.RegistryErr, data.LegacyErr, data.GenerationsErr))
	if changed || availability && completed == cached.completed.Load() {
		readCtx, attempt := cached.diagnostics.start(ctx, changed)
		data = refreshWorkspace(readCtx, entry.StateDBPath, data)
		cached.diagnostics.finish(attempt, errors.Join(data.RegistryErr, data.LegacyErr, data.GenerationsErr))
		if data.LegacyErr == nil && (changed || cached.data.LegacyErr != nil) {
			data.Mtime = stamp.database.mtime
			if stamp.wal.mtime.After(data.Mtime) {
				data.Mtime = stamp.wal.mtime
			}
			data.Revision++
		}
		// Cancellation says nothing about this file state and must not poison it.
		if ctx.Err() == nil {
			cached.stamp, cached.data, cached.known = stamp, data, true
		}
		cached.completed.Add(1)
	}
	data = cloneWorkspaceDiscovery(data)
	data.WorkspaceRoot, data.DescriptorErr = sharedDiscovery.readDescriptor(entry.WorkspaceJSONPath)
	return data
}

func (cache *discoveryCache) workspaceEntry(path string) *workspaceCacheEntry {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if entry := cache.workspaces[path]; entry != nil {
		return entry
	}
	var entry workspaceCacheEntry
	cache.workspaces[path] = &entry
	return &entry
}

func refreshWorkspace(ctx context.Context, path string, data WorkspaceDiscovery) WorkspaceDiscovery {
	db, err := OpenReadOnlyDatabase(ctx, path)
	if err != nil {
		data.RegistryErr, data.LegacyErr, data.GenerationsErr = err, err, err
		return data
	}
	defer func() { _ = db.Close() }()
	registry, _, registryErr := readWorkspaceComposerRegistry(ctx, db)
	legacy, _, legacyErr := ReadLegacyChatData(ctx, db)
	generations, _, generationsErr := ReadGenerationEntries(ctx, db)
	if registryErr == nil {
		data.Registry = registry
	}
	if legacyErr == nil {
		data.Legacy = legacy
	}
	if generationsErr == nil {
		data.Generations = generations
	}
	data.RegistryErr, data.LegacyErr, data.GenerationsErr = registryErr, legacyErr, generationsErr
	return data
}

func cloneWorkspaceDiscovery(data WorkspaceDiscovery) WorkspaceDiscovery {
	data.Registry.AllComposers = slices.Clone(data.Registry.AllComposers)
	data.Legacy.Tabs = slices.Clone(data.Legacy.Tabs)
	for i := range data.Legacy.Tabs {
		data.Legacy.Tabs[i].Bubbles = slices.Clone(data.Legacy.Tabs[i].Bubbles)
	}
	data.Generations = slices.Clone(data.Generations)
	return data
}

func (cache *discoveryCache) readDescriptor(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	cached := cache.descriptorEntry(path)
	completed := cached.completed.Load()
	cached.mu.Lock()
	defer cached.mu.Unlock()
	stamp := readFileMetadata(path)
	if cached.known && cached.stamp == stamp && (!isReadAvailabilityError(cached.err) || completed != cached.completed.Load()) {
		return cached.path, cached.err
	}
	var previousError error
	if cached.stamp == stamp {
		previousError = cached.err
	}
	folder, err := readWorkspaceFolderPath(path, previousError)
	if err != nil {
		folder = cached.path
	}
	cached.stamp, cached.path, cached.err, cached.known = stamp, folder, err, true
	cached.completed.Add(1)
	return folder, err
}

func (cache *discoveryCache) descriptorEntry(path string) *descriptorCacheEntry {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if entry := cache.descriptors[path]; entry != nil {
		return entry
	}
	var entry descriptorCacheEntry
	cache.descriptors[path] = &entry
	return &entry
}

// GlobalDiscovery is the reusable discovery projection of one global store.
// It retains headers and fingerprints, not full bubble payloads.
type GlobalDiscovery struct {
	Headers    map[string]ComposerHeader
	Stocks     map[string]ComposerBubbleStock
	Background []BackgroundComposer
	Metadata   ComposerMetadataIndex
	Err        error
	selectors  map[string]composerBubbleSelector
}

type globalCacheEntry struct {
	completed   atomic.Uint64
	diagnostics cachedReadDiagnostics
	mu          sync.Mutex
	known       bool
	stamp       databaseMetadata
	signature   globalDiscoverySignature
	signed      bool
	data        GlobalDiscovery
}

type globalDiscoverySignature struct {
	composerDigest  [sha256.Size]byte
	bubbleDigest    [sha256.Size]byte
	bubbleRows      int64
	bubbleBytes     int64
	tableLastRow    int64
	tableLastKey    string
	backgroundValue string
	metadataDigest  [sha256.Size]byte
}

func (signature globalDiscoverySignature) sameNonBubble(other globalDiscoverySignature) bool {
	return signature.composerDigest == other.composerDigest &&
		signature.backgroundValue == other.backgroundValue &&
		signature.metadataDigest == other.metadataDigest
}

// ReadGlobalDiscovery refreshes when database/WAL metadata changes or a prior
// availability failure needs retrying. Overlapping consumers share that retry.
// A failed refresh keeps prior contributions; confirmed file absence clears them.
func ReadGlobalDiscovery(ctx context.Context, path string) GlobalDiscovery {
	cached := sharedDiscovery.globalEntry(path)
	completed := cached.completed.Load()
	cached.mu.Lock()
	defer cached.mu.Unlock()
	stamp := readDatabaseMetadata(path)
	changed := !cached.known || cached.stamp != stamp
	availability := isReadAvailabilityError(errors.Join(cached.data.Err, cached.data.Metadata.Err))
	if !changed && (!availability || completed != cached.completed.Load()) {
		return cloneGlobalDiscovery(cached.data)
	}
	var data GlobalDiscovery
	var signature globalDiscoverySignature
	var signatureErr error
	if stamp.database.absent {
		data = GlobalDiscovery{Headers: nil, Stocks: nil, Background: nil, Metadata: ComposerMetadataIndex{ByComposerID: nil, Err: nil}, Err: nil, selectors: nil}
	} else {
		readCtx, attempt := cached.diagnostics.start(ctx, changed)
		var unchanged bool
		data, signature, signatureErr, unchanged = readChangedGlobalDiscovery(readCtx, path, cached)
		if unchanged {
			cached.diagnostics.finish(attempt, nil)
			cached.stamp = stamp
			cached.signature, cached.signed = signature, true
			cached.completed.Add(1)
			return cloneGlobalDiscovery(cached.data)
		}
		cached.diagnostics.finish(attempt, errors.Join(data.Err, data.Metadata.Err))
	}
	if ctx.Err() == nil {
		cached.stamp, cached.data, cached.known = stamp, data, true
		if signatureErr == nil && !stamp.database.absent {
			cached.signature, cached.signed = signature, true
		}
	}
	cached.completed.Add(1)
	return cloneGlobalDiscovery(data)
}

func readChangedGlobalDiscovery(
	ctx context.Context,
	path string,
	cached *globalCacheEntry,
) (GlobalDiscovery, globalDiscoverySignature, error, bool) {
	var signature globalDiscoverySignature
	db, err := OpenReadOnlyDatabase(ctx, path)
	if err != nil {
		data := cached.data
		data.Err = err
		return data, signature, err, false
	}
	defer func() { _ = db.Close() }()
	signature, headers, signatureErr := readGlobalDiscoverySignature(ctx, db, cached.data.Headers)
	if headers == nil {
		headers = cached.data.Headers
	}
	snapshot, err := beginReadSnapshot(ctx, db)
	if err != nil {
		data := cached.data
		data.Err = err
		return data, signature, err, false
	}
	selectorStarted := clock.Now()
	selection := selectComposerBubbleRefresh(ctx, snapshot, cached)
	state := selection.state
	refreshPath := selection.refreshPath
	newRows := selection.newRows
	changedComposerIDs := selection.changedComposerIDs
	selectors := selection.selectors
	selectorQueries := selection.selectorQueries
	selectorErr := selection.err
	if selectorErr == nil {
		signature.bubbleRows = state.rows
		signature.tableLastRow = state.lastRow
		signature.tableLastKey = state.lastKey
		signature.bubbleDigest = selection.digest
		signature.bubbleBytes = selection.bytes
	}
	selectorDuration := clock.Since(selectorStarted)
	signatureErr = errors.Join(signatureErr, selectorErr)
	unchanged := refreshPath == bubbleRefreshPathDelta && signatureErr == nil && cached.known && cached.signed &&
		signature.sameNonBubble(cached.signature) &&
		state.rows == cached.signature.bubbleRows && len(changedComposerIDs) == 0
	if unchanged {
		logComposerStockRefresh(ctx, refreshPath, newRows, 0, selectorDuration, 0, composerStockRefreshStats{
			totalComposers:     len(cached.data.selectors),
			reusedComposers:    len(cached.data.selectors),
			projectedComposers: 0,
			projectedRows:      0,
			projectionQueries:  0,
			selectorQueries:    selectorQueries,
		})
		snapshot.rollback()
		return cached.data, signature, nil, true
	}
	var stocks map[string]ComposerBubbleStock
	var stockStats composerStockRefreshStats
	stockErr := selectorErr
	if stockErr == nil {
		projectionStarted := clock.Now()
		if refreshPath == bubbleRefreshPathFull {
			stocks, selectors, stockStats, stockErr = refreshComposerBubbleStocksInSnapshot(
				ctx, snapshot, selectors, cached.data.Stocks, cached.data.selectors,
			)
		} else {
			stocks, stockStats, stockErr = refreshChangedComposerBubbleStocks(
				ctx, snapshot, changedComposerIDs, selectors, cached.data.Stocks,
			)
		}
		if stockErr == nil {
			stockStats.selectorQueries = selectorQueries
			logComposerStockRefresh(ctx, refreshPath, newRows, len(changedComposerIDs), selectorDuration, clock.Since(projectionStarted), stockStats)
		}
	}
	snapshot.rollback()
	data := refreshGlobalDatabase(ctx, db, cached.data, headers, stocks, selectors, stockErr)
	refreshErr := errors.Join(signatureErr, data.Err, data.Metadata.Err)
	if refreshErr != nil {
		logger := discoveryReadLogger(ctx)
		logger.WarnContext(ctx, "providers.cursor.store.global_refresh_failed", "concern", concern, "err", refreshErr)
	}
	return data, signature, refreshErr, false
}

type bubbleRefreshSelection struct {
	state              bubbleChangeState
	refreshPath        bubbleRefreshPath
	newRows            int64
	changedComposerIDs map[string]bool
	selectors          map[string]composerBubbleSelector
	selectorQueries    int
	digest             [sha256.Size]byte
	bytes              int64
	err                error
}

func selectComposerBubbleRefresh(
	ctx context.Context,
	snapshot readSnapshot,
	cached *globalCacheEntry,
) bubbleRefreshSelection {
	state, err := readBubbleChangeState(ctx, snapshot)
	selection := bubbleRefreshSelection{
		state:              state,
		refreshPath:        bubbleRefreshPathDelta,
		newRows:            0,
		changedComposerIDs: nil,
		selectors:          cached.data.selectors,
		selectorQueries:    3,
		digest:             cached.signature.bubbleDigest,
		bytes:              cached.signature.bubbleBytes,
		err:                err,
	}
	if err != nil {
		return selection
	}
	needsFullReconciliation := !cached.known || !cached.signed ||
		state.rows < cached.signature.bubbleRows ||
		state.lastRow < cached.signature.tableLastRow ||
		state.lastRow == cached.signature.tableLastRow && state.lastKey != cached.signature.tableLastKey
	if needsFullReconciliation {
		return selectFullComposerBubbleRefresh(ctx, snapshot, selection)
	}
	sameChangeSignals := state.rows == cached.signature.bubbleRows &&
		state.lastRow == cached.signature.tableLastRow &&
		state.lastKey == cached.signature.tableLastKey
	if sameChangeSignals {
		selection = selectActiveComposerBubbleRefresh(ctx, snapshot, selection)
		return selectTargetedComposerBubbleRefresh(ctx, snapshot, cached, selection)
	}
	selection.changedComposerIDs, selection.newRows, selection.err = readChangedComposerIDs(
		ctx, snapshot, cached.signature.tableLastRow,
	)
	selection.selectorQueries++
	if selection.err == nil && len(selection.changedComposerIDs) == 0 && state.rows == cached.signature.bubbleRows {
		selection = selectActiveComposerBubbleRefresh(ctx, snapshot, selection)
	}
	return selectTargetedComposerBubbleRefresh(ctx, snapshot, cached, selection)
}

func selectActiveComposerBubbleRefresh(
	ctx context.Context,
	snapshot readSnapshot,
	selection bubbleRefreshSelection,
) bubbleRefreshSelection {
	selection.refreshPath = bubbleRefreshPathActiveInPlace
	activeComposerID, queryCount, err := readMostRecentlyActiveComposerID(ctx, snapshot)
	selection.selectorQueries += queryCount
	selection.err = err
	if err == nil && activeComposerID != "" {
		selection.changedComposerIDs = map[string]bool{activeComposerID: true}
	}
	return selection
}

func selectTargetedComposerBubbleRefresh(
	ctx context.Context,
	snapshot readSnapshot,
	cached *globalCacheEntry,
	selection bubbleRefreshSelection,
) bubbleRefreshSelection {
	if selection.err != nil {
		return selection
	}
	if len(selection.changedComposerIDs) > 0 {
		selection.selectors, selection.selectorQueries, selection.err = refreshChangedComposerBubbleSelectors(
			ctx, snapshot, selection.changedComposerIDs, cached.data.selectors, selection.selectorQueries,
		)
	}
	if selection.err != nil {
		return selection
	}
	expectedRows := expectedBubbleRows(
		cached.signature.bubbleRows,
		selection.changedComposerIDs,
		cached.data.selectors,
		selection.selectors,
	)
	if expectedRows != selection.state.rows {
		return selectFullComposerBubbleRefresh(ctx, snapshot, selection)
	}
	return selection
}

func selectFullComposerBubbleRefresh(
	ctx context.Context,
	snapshot readSnapshot,
	selection bubbleRefreshSelection,
) bubbleRefreshSelection {
	selection.refreshPath = bubbleRefreshPathFull
	inventory, err := readComposerBubbleSelectors(ctx, snapshot)
	selection.selectorQueries += 2
	selection.err = err
	if err == nil {
		selection.selectors = inventory.selectors
		selection.digest = inventory.digest
		selection.bytes = inventory.bytes
	}
	return selection
}

func logComposerStockRefresh(
	ctx context.Context,
	path bubbleRefreshPath,
	newRows int64,
	changedComposers int,
	selectorDuration time.Duration,
	projectionDuration time.Duration,
	stats composerStockRefreshStats,
) {
	discoveryReadLogger(ctx).InfoContext(ctx,
		"providers.cursor.store.composer_stock_refresh_completed",
		"concern", concern,
		"refresh_path", path,
		"new_rows", newRows,
		"changed_composers", changedComposers,
		"selector_duration_ms", selectorDuration.Milliseconds(),
		"projection_duration_ms", projectionDuration.Milliseconds(),
		"total_composers", stats.totalComposers,
		"reused_composers", stats.reusedComposers,
		"projected_composers", stats.projectedComposers,
		"projected_rows", stats.projectedRows,
		"projection_queries", stats.projectionQueries,
		"query_count", stats.selectorQueries+2*stats.projectionQueries,
	)
}

func readGlobalDiscoverySignature(ctx context.Context, db *sql.DB, prior map[string]ComposerHeader) (globalDiscoverySignature, map[string]ComposerHeader, error) {
	var signature globalDiscoverySignature
	headers, conversationErr := readConversationRangeSignature(ctx, db, prior, &signature)
	backgroundErr := readBackgroundSignature(ctx, db, &signature)
	metadataErr := readComposerMetadataSignature(ctx, db, &signature)
	err := errors.Join(conversationErr, backgroundErr, metadataErr)
	if err != nil {
		discoveryReadLogger(ctx).WarnContext(ctx,
			"providers.cursor.store.global_signature_failed",
			"concern", concern,
			"err", err,
		)
	}
	return signature, headers, err
}

func readConversationRangeSignature(ctx context.Context, db *sql.DB, prior map[string]ComposerHeader, signature *globalDiscoverySignature) (map[string]ComposerHeader, error) {
	headers := make(map[string]ComposerHeader, len(prior))
	digest := sha256.New()
	err := forEachKVRowInKeyRange(ctx, db, KVTableCursorDiskKV, keyRangeForPrefix(composerDataKeyPrefix), "", func(row KVRow) error {
		id := strings.TrimPrefix(row.Key, composerDataKeyPrefix)
		writeFingerprintField(digest, strconv.FormatInt(row.RowID, 10))
		writeFingerprintField(digest, row.Key)
		writeFingerprintField(digest, strconv.Itoa(len(row.Value)))
		_, _ = digest.Write(row.Value)
		header, decodeErr := DecodeComposerHeaderJSON(row.Value)
		if decodeErr != nil {
			if previous, found := prior[id]; found {
				headers[id] = previous
			}
			return nil
		}
		header.ComposerID = id
		headers[id] = header
		return nil
	})
	if err != nil {
		return nil, err
	}
	copy(signature.composerDigest[:], digest.Sum(nil))
	return headers, nil
}

func readBackgroundSignature(ctx context.Context, db *sql.DB, signature *globalDiscoverySignature) error {
	value, found, err := ReadKVValue(ctx, db, KVTableItemTable, backgroundComposerWindowMappingKey)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.background_signature_failed", "concern", concern, "err", err)
		return fmt.Errorf("read cursor background signature: %w", err)
	}
	if found {
		signature.backgroundValue = string(value)
	}
	return nil
}

func readComposerMetadataSignature(ctx context.Context, db *sql.DB, signature *globalDiscoverySignature) error {
	exists, err := TableExists(ctx, db, composerHeadersTable)
	if err != nil || !exists {
		return err
	}
	query := "SELECT rowid, COALESCE(composerId, ''), COALESCE(CAST(createdAt AS TEXT), ''), " +
		"COALESCE(CAST(lastUpdatedAt AS TEXT), ''), COALESCE(CAST(isArchived AS TEXT), ''), " +
		"COALESCE(CAST(isSubagent AS TEXT), ''), json_valid(value), " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.name') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.subtitle') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.isArchived') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.workspaceIdentifier.uri.fsPath') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.workspaceIdentifier.uri.path') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.workspaceIdentifier.configPath.fsPath') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.workspaceIdentifier.configPath.path') AS TEXT), '') ELSE '' END " +
		"FROM composerHeaders ORDER BY rowid"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_metadata_signature_failed", "concern", concern, "err", err)
		return fmt.Errorf("read cursor composer metadata signature: %w", err)
	}
	defer func() { _ = rows.Close() }()
	digest := sha256.New()
	for rows.Next() {
		var rowID, valid int64
		var composerID, createdAt, lastUpdatedAt, archived, subagent, name, subtitle, jsonArchived, uriFsPath, uriPath, configFsPath, configPath string
		if err := rows.Scan(&rowID, &composerID, &createdAt, &lastUpdatedAt, &archived, &subagent, &valid, &name, &subtitle, &jsonArchived, &uriFsPath, &uriPath, &configFsPath, &configPath); err != nil {
			return fmt.Errorf("scan cursor composer metadata signature: %w", err)
		}
		for _, field := range []string{strconv.FormatInt(rowID, 10), composerID, createdAt, lastUpdatedAt, archived, subagent, strconv.FormatInt(valid, 10), name, subtitle, jsonArchived, uriFsPath, uriPath, configFsPath, configPath} {
			writeFingerprintField(digest, field)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate cursor composer metadata signature: %w", err)
	}
	copy(signature.metadataDigest[:], digest.Sum(nil))
	return nil
}

func (cache *discoveryCache) globalEntry(path string) *globalCacheEntry {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if entry := cache.globals[path]; entry != nil {
		return entry
	}
	var entry globalCacheEntry
	cache.globals[path] = &entry
	return &entry
}

func refreshGlobalDatabase(
	ctx context.Context,
	db *sql.DB,
	data GlobalDiscovery,
	headers map[string]ComposerHeader,
	stocks map[string]ComposerBubbleStock,
	selectors map[string]composerBubbleSelector,
	stockErr error,
) GlobalDiscovery {
	background, backgroundErr := ListBackgroundComposers(ctx, db)
	metadata, metadataErr := ReadComposerMetadataIndex(ctx, db)
	data.Headers = headers
	if stockErr == nil {
		data.Stocks = stocks
		data.selectors = selectors
	}
	if backgroundErr == nil {
		data.Background = background
	}
	if metadataErr == nil {
		data.Metadata.ByComposerID = metadata
	}
	data.Metadata.Err = metadataErr
	data.Err = errors.Join(stockErr, backgroundErr)
	return data
}

func cloneGlobalDiscovery(data GlobalDiscovery) GlobalDiscovery {
	data.Headers = maps.Clone(data.Headers)
	for id, header := range data.Headers {
		header.FullConversationHeadersOnly = slices.Clone(header.FullConversationHeadersOnly)
		data.Headers[id] = header
	}
	data.Stocks = maps.Clone(data.Stocks)
	data.selectors = maps.Clone(data.selectors)
	data.Background = slices.Clone(data.Background)
	data.Metadata.ByComposerID = maps.Clone(data.Metadata.ByComposerID)
	return data
}
