package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/gklog/trace"
)

const (
	cacheFilename   = "conversation-index.json"
	refreshDebounce = 30 * time.Second
)

// Index owns the derived raw conversation cache. It resolves each artifact's
// parser through the registry it is constructed with, so the daemon wires the
// provider parsers in once and every read and scan dispatches through them.
type Index struct {
	mu           sync.Mutex
	registry     *Registry
	records      []Record
	prevRecords  map[string]Record
	prevStamps   map[string]FileStamp
	loaded       bool
	refreshing   bool
	lastRefresh  time.Time
	cachePath    string
	debounce     time.Duration
	scanProvider func(context.Context, *Registry, scanCache) (scanResult, error)
}

// NewIndex returns an index backed by the default XDG cache path that resolves
// artifacts through registry. The daemon registers the Claude and Codex parsers
// before constructing the index.
func NewIndex(registry *Registry) *Index {
	return &Index{
		mu:           sync.Mutex{},
		registry:     registry,
		records:      nil,
		prevRecords:  nil,
		prevStamps:   nil,
		loaded:       false,
		refreshing:   false,
		lastRefresh:  time.Time{},
		cachePath:    filepath.Join(config.GlobalCacheDir(), cacheFilename),
		debounce:     refreshDebounce,
		scanProvider: scan,
	}
}

// Start runs a periodic debounced cache refresh until ctx is canceled.
func (idx *Index) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	// Load the cached records and their file stamps before the first refresh so
	// the worker reuses unchanged transcripts and re-parses only what changed,
	// rather than re-reading the whole corpus on every start.
	if err := idx.loadOnce(); err != nil {
		slog.WarnContext(ctx, "conversation.index.start_load_failed", "concern", "conversation.index", "component", "conversation", "err", err)
	}
	idx.refreshAsync(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			idx.refreshAsync(ctx)
		}
	}
}

// List returns the cached records and starts a debounced background refresh.
func (idx *Index) List(ctx context.Context) ([]Record, error) {
	if err := idx.loadOnce(); err != nil {
		return nil, err
	}
	idx.refreshAsync(ctx)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return cloneRecords(idx.records), nil
}

// ListWithStamps returns the cached records with their artifact file stamps and
// starts a debounced background refresh.
func (idx *Index) ListWithStamps(ctx context.Context) ([]StampedRecord, error) {
	if err := idx.loadOnce(); err != nil {
		return nil, err
	}
	idx.refreshAsync(ctx)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return cloneStampedRecords(idx.records, idx.prevStamps), nil
}

// RecordByID returns the cached record with the exact id from the in-memory
// snapshot, taking no refresh. The second result is false when no record
// matches.
func (idx *Index) RecordByID(id string) (Record, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return emptyRecord(), false
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, record := range idx.records {
		if record.ID == id {
			return record, true
		}
	}
	return emptyRecord(), false
}

func (idx *Index) recordsSnapshot() []Record {
	idx.mu.Lock()
	records := cloneRecords(idx.records)
	idx.mu.Unlock()
	return records
}

// Resolve finds one conversation by id, native id, title, or artifact path.
func (idx *Index) Resolve(ctx context.Context, selector string) (Record, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return Record{}, errors.New("conversation selector is required")
	}
	records, err := idx.List(ctx)
	if err != nil {
		return Record{}, err
	}
	if record, ok := resolveRecord(records, selector); ok {
		return record, nil
	}
	if err := idx.Refresh(ctx); err != nil {
		return Record{}, err
	}
	records = idx.recordsSnapshot()
	if record, ok := resolveRecord(records, selector); ok {
		return record, nil
	}
	return Record{}, fmt.Errorf("conversation %q not found", selector)
}

// Refresh rebuilds the cache synchronously.
func (idx *Index) Refresh(ctx context.Context) (err error) {
	defer trace.Op(ctx, "conversation.index.refresh")(&err)
	idx.mu.Lock()
	if idx.refreshing {
		idx.mu.Unlock()
		return nil
	}
	idx.refreshing = true
	prior := scanCache{records: idx.prevRecords, stamps: idx.prevStamps}
	idx.mu.Unlock()

	result, err := idx.scanProvider(ctx, idx.registry, prior)
	if err != nil {
		idx.mu.Lock()
		idx.refreshing = false
		idx.mu.Unlock()
		return err
	}
	sortRecords(result.records)
	if err := writeCache(idx.cachePath, result.records, result.stamps); err != nil {
		idx.mu.Lock()
		idx.refreshing = false
		idx.mu.Unlock()
		return err
	}
	idx.mu.Lock()
	idx.records = result.records
	idx.prevRecords = recordsByPath(result.records)
	idx.prevStamps = result.stamps
	idx.loaded = true
	idx.refreshing = false
	idx.lastRefresh = clock.Now()
	idx.mu.Unlock()
	return nil
}

func (idx *Index) loadOnce() error {
	idx.mu.Lock()
	if idx.loaded {
		idx.mu.Unlock()
		return nil
	}
	idx.mu.Unlock()

	records, stamps, err := readCache(idx.cachePath)
	if err != nil {
		return err
	}
	idx.mu.Lock()
	idx.records = records
	idx.prevRecords = recordsByPath(records)
	idx.prevStamps = stamps
	idx.loaded = true
	idx.mu.Unlock()
	return nil
}

func (idx *Index) refreshAsync(ctx context.Context) {
	idx.mu.Lock()
	if idx.refreshing || clock.Now().Sub(idx.lastRefresh) < idx.debounce {
		idx.mu.Unlock()
		return
	}
	idx.refreshing = true
	prior := scanCache{records: idx.prevRecords, stamps: idx.prevStamps}
	idx.mu.Unlock()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(ctx, "conversation.index.refresh_panic", "concern", "conversation.index", "component", "conversation", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		result, err := idx.scanProvider(context.WithoutCancel(ctx), idx.registry, prior)
		if err == nil {
			sortRecords(result.records)
			err = writeCache(idx.cachePath, result.records, result.stamps)
		}
		idx.mu.Lock()
		defer idx.mu.Unlock()
		if err == nil {
			idx.records = result.records
			idx.prevRecords = recordsByPath(result.records)
			idx.prevStamps = result.stamps
			idx.loaded = true
			idx.lastRefresh = clock.Now()
		}
		idx.refreshing = false
	}()
}

// recordsByPath indexes records by artifact path so the next incremental scan
// can reuse the record for any file whose stamp is unchanged.
func recordsByPath(records []Record) map[string]Record {
	byPath := make(map[string]Record, len(records))
	for _, record := range records {
		byPath[record.ArtifactPath] = record
	}
	return byPath
}

// cacheFile is the on-disk index cache. It persists the per-file stamps next to
// the records so a freshly started worker reuses them on its first refresh and
// re-parses only changed transcripts, instead of re-reading the whole corpus on
// every start.
type cacheFile struct {
	Records []Record             `json:"records"`
	Stamps  map[string]FileStamp `json:"stamps"`
}

func readCache(path string) ([]Record, map[string]FileStamp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		slog.Warn("conversation.index.cache_read_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return nil, nil, fmt.Errorf("read conversation cache: %w", err)
	}
	var cache cacheFile
	if err := json.Unmarshal(data, &cache); err == nil && cache.Records != nil {
		sortRecords(cache.Records)
		return cache.Records, cache.Stamps, nil
	}
	// Fall back to the legacy records-only array. The next write upgrades the
	// file to the stamped envelope, so the first refresh re-parses once and then
	// the startup path is cheap.
	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		slog.Warn("conversation.index.cache_decode_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return nil, nil, fmt.Errorf("decode conversation cache: %w", err)
	}
	sortRecords(records)
	return records, nil, nil
}

func writeCache(path string, records []Record, stamps map[string]FileStamp) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("conversation.index.cache_mkdir_failed", "concern", "conversation.index", "component", "conversation", "path", filepath.Dir(path), "err", err)
		return fmt.Errorf("create conversation cache dir: %w", err)
	}
	data, err := json.MarshalIndent(cacheFile{Records: records, Stamps: stamps}, "", "  ")
	if err != nil {
		slog.Warn("conversation.index.cache_encode_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return fmt.Errorf("encode conversation cache: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		slog.Warn("conversation.index.cache_write_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return fmt.Errorf("write conversation cache: %w", err)
	}
	slog.Debug("conversation.index.cache_written", "concern", "conversation.index", "component", "conversation", "path", path, "records", len(records))
	return nil
}

func resolveRecord(records []Record, selector string) (Record, bool) {
	for _, record := range records {
		if record.ID == selector ||
			record.NativeID == selector ||
			record.Title == selector ||
			record.ArtifactPath == selector {
			return record, true
		}
	}
	lower := strings.ToLower(selector)
	var matched Record
	matchCount := 0
	for _, record := range records {
		if strings.Contains(strings.ToLower(record.Title), lower) {
			matched = record
			matchCount++
		}
	}
	return matched, matchCount == 1
}

func sortRecords(records []Record) {
	sort.SliceStable(records, func(i int, j int) bool {
		left := records[i]
		right := records[j]
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		return left.ID < right.ID
	})
}

func cloneRecords(records []Record) []Record {
	if len(records) == 0 {
		return nil
	}
	out := make([]Record, len(records))
	copy(out, records)
	return out
}

func cloneStampedRecords(records []Record, stamps map[string]FileStamp) []StampedRecord {
	if len(records) == 0 {
		return nil
	}
	out := make([]StampedRecord, 0, len(records))
	for _, record := range records {
		out = append(out, StampedRecord{
			Record: record,
			Stamp:  stamps[record.ArtifactPath],
		})
	}
	return out
}

// DerivedID returns the stable conversation id for an artifact: the provider
// label joined to the native provider id, or an artifact-hash id when the
// native id is missing. The provider parser packages call it so every record
// shares one id derivation.
func DerivedID(provider Provider, providerID string, artifactPath string) string {
	providerID = strings.TrimSpace(providerID)
	if providerID != "" {
		return provider.String() + ":" + providerID
	}
	sum := sha256.Sum256([]byte(artifactPath))
	return ProviderArtifact.String() + ":" + hex.EncodeToString(sum[:8])
}
