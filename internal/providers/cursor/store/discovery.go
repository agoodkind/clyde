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
	"sync"
	"sync/atomic"
	"time"
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
	bubbleRows      int64
	bubbleLastRow   int64
	bubbleBytes     int64
	backgroundValue string
	metadataDigest  [sha256.Size]byte
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
		data = GlobalDiscovery{Headers: nil, Stocks: nil, Background: nil, Metadata: ComposerMetadataIndex{ByComposerID: nil, Err: nil}, Err: nil}
	} else {
		readCtx, attempt := cached.diagnostics.start(ctx, changed)
		var unchanged bool
		data, signature, signatureErr, unchanged = readChangedGlobalDiscovery(readCtx, path, cached)
		if unchanged {
			cached.diagnostics.finish(attempt, nil)
			cached.stamp = stamp
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
	signature, signatureErr := readGlobalDiscoverySignature(ctx, db)
	unchanged := signatureErr == nil && cached.known && cached.signed && signature == cached.signature
	if unchanged {
		return cached.data, signature, nil, true
	}
	return refreshGlobalDatabase(ctx, db, cached.data), signature, signatureErr, false
}

func readGlobalDiscoverySignature(ctx context.Context, db *sql.DB) (globalDiscoverySignature, error) {
	var signature globalDiscoverySignature
	if err := readConversationRangeSignature(ctx, db, &signature); err != nil {
		return signature, err
	}
	if err := readBackgroundSignature(ctx, db, &signature); err != nil {
		return signature, err
	}
	if err := readComposerMetadataSignature(ctx, db, &signature); err != nil {
		return signature, err
	}
	return signature, nil
}

func readConversationRangeSignature(ctx context.Context, db *sql.DB, signature *globalDiscoverySignature) error {
	composerBounds := keyRangeForPrefix(composerDataKeyPrefix)
	query := "SELECT rowid, key, COALESCE(length(value), -1), json_valid(value), " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.name') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.createdAt') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.lastUpdatedAt') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.status') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.unifiedMode') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.forceMode') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.latestChatGenerationUUID') AS TEXT), '') ELSE '' END, " +
		"CASE WHEN json_valid(value) THEN COALESCE(CAST(json_extract(value, '$.fullConversationHeadersOnly') AS TEXT), '') ELSE '' END " +
		"FROM cursorDiskKV WHERE key >= ? AND key < ? ORDER BY key"
	rows, err := db.QueryContext(ctx, query, composerBounds.Lower, composerBounds.Upper)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_signature_failed", "concern", concern, "err", err)
		return fmt.Errorf("read cursor composer signature: %w", err)
	}
	defer func() { _ = rows.Close() }()
	digest := sha256.New()
	for rows.Next() {
		var rowID, valueLength, valid int64
		var key, name, createdAt, lastUpdatedAt, status, unifiedMode, forceMode, requestID, headers string
		if err := rows.Scan(&rowID, &key, &valueLength, &valid, &name, &createdAt, &lastUpdatedAt, &status, &unifiedMode, &forceMode, &requestID, &headers); err != nil {
			return fmt.Errorf("scan cursor composer signature: %w", err)
		}
		for _, field := range []string{strconv.FormatInt(rowID, 10), key, strconv.FormatInt(valueLength, 10), strconv.FormatInt(valid, 10), name, createdAt, lastUpdatedAt, status, unifiedMode, forceMode, requestID, headers} {
			writeFingerprintField(digest, field)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate cursor composer signature: %w", err)
	}
	copy(signature.composerDigest[:], digest.Sum(nil))

	bubbleBounds := keyRangeForPrefix(bubbleKeyPrefix)
	query = "SELECT count(*), COALESCE(max(rowid), 0), COALESCE(sum(length(value)), 0) FROM cursorDiskKV WHERE " +
		"key >= ? AND key < ?"
	err = db.QueryRowContext(ctx, query,
		bubbleBounds.Lower, bubbleBounds.Upper,
	).Scan(&signature.bubbleRows, &signature.bubbleLastRow, &signature.bubbleBytes)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.conversation_signature_failed", "concern", concern, "err", err)
		return fmt.Errorf("read cursor conversation signature: %w", err)
	}
	return nil
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

func refreshGlobalDatabase(ctx context.Context, db *sql.DB, data GlobalDiscovery) GlobalDiscovery {
	headers, headerErr := readComposerHeaders(ctx, db, data.Headers)
	stocks, stockErr := ReadComposerBubbleStocks(ctx, db)
	background, backgroundErr := ListBackgroundComposers(ctx, db)
	metadata, metadataErr := ReadComposerMetadataIndex(ctx, db)
	if headerErr == nil {
		data.Headers = headers
	}
	if stockErr == nil {
		data.Stocks = stocks
	}
	if backgroundErr == nil {
		data.Background = background
	}
	if metadataErr == nil {
		data.Metadata.ByComposerID = metadata
	}
	data.Metadata.Err = metadataErr
	data.Err = errors.Join(headerErr, stockErr, backgroundErr)
	return data
}

func cloneGlobalDiscovery(data GlobalDiscovery) GlobalDiscovery {
	data.Headers = maps.Clone(data.Headers)
	for id, header := range data.Headers {
		header.FullConversationHeadersOnly = slices.Clone(header.FullConversationHeadersOnly)
		data.Headers[id] = header
	}
	data.Stocks = maps.Clone(data.Stocks)
	data.Background = slices.Clone(data.Background)
	data.Metadata.ByComposerID = maps.Clone(data.Metadata.ByComposerID)
	return data
}
