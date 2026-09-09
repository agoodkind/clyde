package cursorstore

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
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
	data        GlobalDiscovery
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
	if stamp.database.absent {
		data = GlobalDiscovery{Headers: nil, Stocks: nil, Background: nil, Metadata: ComposerMetadataIndex{ByComposerID: nil, Err: nil}, Err: nil}
	} else {
		readCtx, attempt := cached.diagnostics.start(ctx, changed)
		data = refreshGlobal(readCtx, path, cached.data)
		cached.diagnostics.finish(attempt, errors.Join(data.Err, data.Metadata.Err))
	}
	if ctx.Err() == nil {
		cached.stamp, cached.data, cached.known = stamp, data, true
	}
	cached.completed.Add(1)
	return cloneGlobalDiscovery(data)
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

func refreshGlobal(ctx context.Context, path string, data GlobalDiscovery) GlobalDiscovery {
	db, err := OpenReadOnlyDatabase(ctx, path)
	if err != nil {
		data.Err = err
		return data
	}
	defer func() { _ = db.Close() }()
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
