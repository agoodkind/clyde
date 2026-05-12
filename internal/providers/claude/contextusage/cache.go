package contextusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"goodkind.io/clyde/internal/contextusage"
)

// Cache TTL constants. These are upper bounds; callers that want a
// stricter age pass UsageOptions.MaxAge. Invalidation on transcript
// mtime change happens regardless of TTL.
const (
	memoryCacheTTL    = 30 * time.Second
	diskCacheTTL      = 5 * time.Minute
	diskCacheSchemaV  = 1
	diskCacheFilename = "context.json"
)

// cachedUsage is the on-disk cache envelope. Stored as JSON at
// $XDG_STATE_HOME/clyde/sessions/<id>/context.json. SchemaVersion
// gates upgrades so a clyde binary reading an older file rejects
// rather than silently misinterpreting fields.
type cachedUsage struct {
	SchemaVersion   int                   `json:"schema_version"`
	Usage           contextusage.Snapshot `json:"usage"`
	CapturedAt      time.Time             `json:"captured_at"`
	TranscriptMTime int64                 `json:"transcript_mtime_ns"`
	TranscriptPath  string                `json:"transcript_path"`
}

// memoryCache is a single-session, single-process sync.Map wrapper.
// The layer holds one per session so rapid iterations (for example
// the planner target loop calling Usage before each candidate count)
// do not re-read disk or re-probe.
type memoryCache struct {
	mu       sync.Mutex
	payload  *cachedUsage
	loadedAt time.Time
	ttl      time.Duration
}

func newMemoryCache() *memoryCache {
	return &memoryCache{ttl: memoryCacheTTL}
}

// get returns the payload when it satisfies both the TTL and
// opts.MaxAge, and when the transcript's on-disk mtime has not
// advanced past CapturedAt. A nil return signals miss, not error.
func (c *memoryCache) get(transcriptPath string, opts UsageOptions) *cachedUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.payload == nil {
		return nil
	}
	age := time.Since(c.loadedAt)
	if age > c.ttl {
		return nil
	}
	if opts.MaxAge > 0 && age > opts.MaxAge {
		return nil
	}
	if mtimeChanged(transcriptPath, c.payload.TranscriptMTime) {
		return nil
	}
	return c.payload
}

func (c *memoryCache) put(payload *cachedUsage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payload = payload
	c.loadedAt = currentTime()
}

func (c *memoryCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payload = nil
	c.loadedAt = time.Time{}
}

// diskCache reads and writes the per-session context.json. Reads
// tolerate missing files (fresh session, never probed). Writes are
// atomic via *.tmp plus rename so a partial write never poisons
// subsequent reads.
type diskCache struct {
	path           string
	ttl            time.Duration
	transcriptPath string
}

func newDiskCache(sessionID, transcriptPath, stateDir string) *diskCache {
	return &diskCache{
		path:           filepath.Join(stateDir, "sessions", sessionID, diskCacheFilename),
		ttl:            diskCacheTTL,
		transcriptPath: transcriptPath,
	}
}

// read returns the on-disk cached payload when it satisfies TTL,
// opts.MaxAge, the schema version, and the transcript mtime match.
// (nil, nil) means cache miss without error. A non-nil error signals
// the cache file exists but is corrupt or unreadable; the caller logs
// and falls through to a fresh probe.
func (d *diskCache) read(opts UsageOptions) (*cachedUsage, error) {
	data, err := os.ReadFile(d.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		contextLog.Warn("session.context.disk_cache.read_file_failed",
			"component", "contextusage",
			"subcomponent", "disk_cache",
			"path", d.path,
			"err", err,
		)
		return nil, fmt.Errorf("diskcache read: %w", err)
	}
	var payload cachedUsage
	if err := json.Unmarshal(data, &payload); err != nil {
		contextLog.Warn("session.context.disk_cache.decode_failed",
			"component", "contextusage",
			"subcomponent", "disk_cache",
			"path", d.path,
			"err", err,
		)
		return nil, fmt.Errorf("diskcache decode: %w", err)
	}
	if payload.SchemaVersion != diskCacheSchemaV {
		sessionContextLog.Logger().Debug("session.context.disk_cache.schema_mismatch",
			"component", "contextusage",
			"subcomponent", "disk_cache",
			"got", payload.SchemaVersion,
			"want", diskCacheSchemaV,
		)
		return nil, nil
	}
	age := time.Since(payload.CapturedAt)
	if age > d.ttl {
		return nil, nil
	}
	if opts.MaxAge > 0 && age > opts.MaxAge {
		return nil, nil
	}
	if mtimeChanged(d.transcriptPath, payload.TranscriptMTime) {
		return nil, nil
	}
	return &payload, nil
}

// write persists a fresh probe result atomically. Failures log but do
// not surface to the caller because the in-memory cache already has
// the fresh value; a missed disk write only hurts the next process.
func (d *diskCache) write(payload *cachedUsage) {
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		contextLog.Warn("session.context.disk_cache.mkdir_failed",
			"component", "contextusage",
			"subcomponent", "disk_cache",
			"path", d.path,
			"err", err,
		)
		return
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		contextLog.Warn("session.context.disk_cache.encode_failed",
			"component", "contextusage",
			"subcomponent", "disk_cache",
			"err", err,
		)
		return
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o644); err != nil {
		contextLog.Warn("session.context.disk_cache.write_failed",
			"component", "contextusage",
			"subcomponent", "disk_cache",
			"path", tmp,
			"err", err,
		)
		return
	}
	if err := os.Rename(tmp, d.path); err != nil {
		_ = os.Remove(tmp)
		contextLog.Warn("session.context.disk_cache.rename_failed",
			"component", "contextusage",
			"subcomponent", "disk_cache",
			"path", d.path,
			"err", err,
		)
		return
	}
	sessionContextLog.Logger().Debug("session.context.disk_cache.write",
		"component", "contextusage",
		"subcomponent", "disk_cache",
		"path", d.path,
		"bytes", len(encoded),
	)
}

func (d *diskCache) invalidate() {
	if err := os.Remove(d.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		contextLog.Warn("session.context.disk_cache.invalidate_failed",
			"component", "contextusage",
			"subcomponent", "disk_cache",
			"path", d.path,
			"err", err,
		)
		return
	}
	sessionContextLog.Logger().Debug("session.context.disk_cache.invalidated",
		"component", "contextusage",
		"subcomponent", "disk_cache",
		"path", d.path,
	)
}

// mtimeChanged returns true when transcriptPath's on-disk
// modification time is later than the cached value. The function
// returns false for missing or unreadable paths so cache lookups do
// not spuriously invalidate when the transcript was never materialized
// (new session) or temporarily unreadable.
func mtimeChanged(transcriptPath string, cachedMTimeNs int64) bool {
	if transcriptPath == "" {
		return false
	}
	info, err := os.Stat(transcriptPath)
	if err != nil {
		return false
	}
	return info.ModTime().UnixNano() > cachedMTimeNs
}

// transcriptMTimeNs returns the current mtime in nanoseconds, or zero
// when the file is missing or unreadable. Used at probe-result-capture
// time so the cache payload records what mtime the result was valid
// against.
func transcriptMTimeNs(transcriptPath string) int64 {
	if transcriptPath == "" {
		return 0
	}
	info, err := os.Stat(transcriptPath)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}
