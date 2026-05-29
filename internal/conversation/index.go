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

// Index owns the derived raw conversation cache.
type Index struct {
	mu           sync.Mutex
	records      []Record
	loaded       bool
	refreshing   bool
	lastRefresh  time.Time
	cachePath    string
	debounce     time.Duration
	scanProvider func(context.Context) ([]Record, error)
}

// NewIndex returns an index backed by the default XDG cache path.
func NewIndex() *Index {
	return &Index{
		mu:           sync.Mutex{},
		records:      nil,
		loaded:       false,
		refreshing:   false,
		lastRefresh:  time.Time{},
		cachePath:    filepath.Join(config.GlobalCacheDir(), cacheFilename),
		debounce:     refreshDebounce,
		scanProvider: Scan,
	}
}

// DefaultIndex is the process-wide conversation index used by CLI and MCP.
var DefaultIndex = NewIndex()

// Start runs a periodic debounced cache refresh until ctx is canceled.
func (idx *Index) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
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
	idx.mu.Unlock()

	records, err := idx.scanProvider(ctx)
	if err != nil {
		idx.mu.Lock()
		idx.refreshing = false
		idx.mu.Unlock()
		return err
	}
	sortRecords(records)
	if err := writeCache(idx.cachePath, records); err != nil {
		idx.mu.Lock()
		idx.refreshing = false
		idx.mu.Unlock()
		return err
	}
	idx.mu.Lock()
	idx.records = records
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

	records, err := readCache(idx.cachePath)
	if err != nil {
		return err
	}
	idx.mu.Lock()
	idx.records = records
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
	idx.mu.Unlock()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(ctx, "conversation.index.refresh_panic", "concern", "conversation.index", "component", "conversation", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		records, err := idx.scanProvider(context.WithoutCancel(ctx))
		if err == nil {
			sortRecords(records)
			err = writeCache(idx.cachePath, records)
		}
		idx.mu.Lock()
		defer idx.mu.Unlock()
		if err == nil {
			idx.records = records
			idx.loaded = true
			idx.lastRefresh = clock.Now()
		}
		idx.refreshing = false
	}()
}

func readCache(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		slog.Warn("conversation.index.cache_read_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return nil, fmt.Errorf("read conversation cache: %w", err)
	}
	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		slog.Warn("conversation.index.cache_decode_failed", "concern", "conversation.index", "component", "conversation", "path", path, "err", err)
		return nil, fmt.Errorf("decode conversation cache: %w", err)
	}
	sortRecords(records)
	return records, nil
}

func writeCache(path string, records []Record) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("conversation.index.cache_mkdir_failed", "concern", "conversation.index", "component", "conversation", "path", filepath.Dir(path), "err", err)
		return fmt.Errorf("create conversation cache dir: %w", err)
	}
	data, err := json.MarshalIndent(records, "", "  ")
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

func derivedID(provider Provider, providerID string, artifactPath string) string {
	providerID = strings.TrimSpace(providerID)
	if providerID != "" {
		return provider.String() + ":" + providerID
	}
	sum := sha256.Sum256([]byte(artifactPath))
	return ProviderArtifact.String() + ":" + hex.EncodeToString(sum[:8])
}
