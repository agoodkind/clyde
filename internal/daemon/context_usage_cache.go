package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/session"
)

var (
	errContextUsageSessionUnavailable = errors.New("context usage session unavailable")
	// errContextUsageCooldown is returned by Refresh when a previous probe
	// for the same session failed recently and the cooldown window has
	// not elapsed. Callers should treat this as a soft "try again later"
	// rather than a probe failure.
	errContextUsageCooldown = errors.New("context usage cooldown active")
)

type contextUsageProbeFunc func(context.Context, *session.Session) (sessionContextState, error)

// contextUsageCacheKey identifies a unique snapshot of a session's
// context usage. The transcript file is append-only JSONL, so the file
// size alone is a reliable freshness signal: if the size has not grown,
// no new chat messages exist and the cached usage is still correct.
//
// File mod time is intentionally NOT part of the key. Mod time can drift
// without content change (touch, rsync, atomic same-size renames) and
// would otherwise trigger spurious re-probes.
type contextUsageCacheKey struct {
	SessionName         string
	Provider            string
	SessionID           string
	TranscriptPath      string
	TranscriptSizeBytes int64
}

type contextUsageCacheEntry struct {
	State    sessionContextState
	CachedAt time.Time
}

type contextUsageInflight struct {
	Done  chan struct{}
	State sessionContextState
	Err   error
}

// contextUsageStateCache is the simple, lazy, debounced, idempotent
// per-session cache for context usage. Entries stay valid as long as
// the transcript size for that session is unchanged. A failed probe
// puts the session into a per-session cooldown so a hot loop cannot
// re-spawn probes after a transient error. The cooldown is the only
// time-based gate; successful entries do not expire on a clock.
type contextUsageStateCache struct {
	mu sync.Mutex
	// cooldown is the minimum interval between probe attempts for a
	// session whose previous probe failed. Successful probes do not
	// use this gate; their replacement is governed by transcript size.
	cooldown time.Duration
	states   map[contextUsageCacheKey]contextUsageCacheEntry
	inflight map[contextUsageCacheKey]*contextUsageInflight
	// failures records the wall-clock time of the most recent failed
	// probe per session name. A session in cooldown returns
	// errContextUsageCooldown from Refresh until the cooldown elapses.
	failures map[string]time.Time
}

func newContextUsageStateCache(cooldown time.Duration) *contextUsageStateCache {
	cache := new(contextUsageStateCache)
	cache.cooldown = cooldown
	cache.states = make(map[contextUsageCacheKey]contextUsageCacheEntry)
	cache.inflight = make(map[contextUsageCacheKey]*contextUsageInflight)
	cache.failures = make(map[string]time.Time)
	return cache
}

// Get returns the cached state for sess if one exists. The boolean
// return is true on cache hit. Entries are not expired by clock; only
// a transcript size change makes them un-findable on the new key.
func (c *contextUsageStateCache) Get(sess *session.Session) (sessionContextState, bool) {
	if c == nil {
		return emptySessionContextState(), false
	}
	key, ok := contextUsageKeyForSession(sess)
	if !ok {
		return emptySessionContextState(), false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(key)
}

// Refresh returns a cached entry if available, otherwise it runs probe
// once for sess. Concurrent Refresh calls for the same key collapse
// onto a single in-flight probe via the inflight map. A session in the
// failure cooldown window returns errContextUsageCooldown without
// running probe so a hot loop cannot reactivate spawning.
func (c *contextUsageStateCache) Refresh(ctx context.Context, sess *session.Session, now time.Time, probe contextUsageProbeFunc) (sessionContextState, error) {
	key, ok := contextUsageKeyForSession(sess)
	if !ok {
		return emptySessionContextState(), errContextUsageSessionUnavailable
	}
	c.mu.Lock()
	if state, hit := c.getLocked(key); hit {
		c.mu.Unlock()
		return state, nil
	}
	if running := c.inflight[key]; running != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "daemon.context_usage_cache.wait_cancelled", "err", ctx.Err())
			return emptySessionContextState(), fmt.Errorf("wait context usage refresh: %w", ctx.Err())
		case <-running.Done:
			return running.State, running.Err
		}
	}
	if last, found := c.failures[sess.Name]; found && c.cooldown > 0 && now.Sub(last) < c.cooldown {
		c.mu.Unlock()
		state := emptySessionContextState()
		state.Status = "cooldown"
		return state, errContextUsageCooldown
	}
	running := &contextUsageInflight{
		Done:  make(chan struct{}),
		State: emptySessionContextState(),
		Err:   nil,
	}
	c.inflight[key] = running
	c.mu.Unlock()

	probeStart := now
	slog.InfoContext(ctx, "daemon.context_usage.probe_started",
		"component", "daemon",
		"session", sess.Name,
		"transcript_size_bytes", key.TranscriptSizeBytes,
	)
	state, err := probe(ctx, sess)
	probeDuration := time.Since(probeStart)
	if err == nil {
		state.Loaded = true
		state.Refreshing = false
		state.Status = ""
	}

	c.mu.Lock()
	if err == nil {
		c.evictOtherSessionEntriesLocked(key)
		c.states[key] = contextUsageCacheEntry{
			State:    state,
			CachedAt: now,
		}
		delete(c.failures, sess.Name)
	} else {
		c.failures[sess.Name] = now
	}
	running.State = state
	running.Err = err
	delete(c.inflight, key)
	close(running.Done)
	c.mu.Unlock()

	if err == nil {
		slog.InfoContext(ctx, "daemon.context_usage.probe_completed",
			"component", "daemon",
			"session", sess.Name,
			"duration_ms", probeDuration.Milliseconds(),
			"total_tokens", state.Usage.TotalTokens,
			"max_tokens", state.Usage.MaxTokens,
		)
	} else {
		slog.WarnContext(ctx, "daemon.context_usage.probe_completed",
			"component", "daemon",
			"session", sess.Name,
			"duration_ms", probeDuration.Milliseconds(),
			"err", err,
		)
	}
	return state, err
}

// RenameSession migrates cache entries and any active failure cooldown
// from oldName to newName when a session is renamed.
func (c *contextUsageStateCache) RenameSession(oldName, newName string) {
	if c == nil || oldName == "" || newName == "" || oldName == newName {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.states {
		if key.SessionName != oldName {
			continue
		}
		delete(c.states, key)
		key.SessionName = newName
		c.states[key] = entry
	}
	if when, ok := c.failures[oldName]; ok {
		delete(c.failures, oldName)
		c.failures[newName] = when
	}
}

// DeleteSession drops every cache entry, in-flight handle, and failure
// cooldown attached to the given session name.
func (c *contextUsageStateCache) DeleteSession(name string) {
	if c == nil || name == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.states {
		if key.SessionName == name {
			delete(c.states, key)
		}
	}
	delete(c.failures, name)
}

func (c *contextUsageStateCache) getLocked(key contextUsageCacheKey) (sessionContextState, bool) {
	entry, ok := c.states[key]
	if !ok {
		return emptySessionContextState(), false
	}
	return entry.State, true
}

// evictOtherSessionEntriesLocked removes every existing entry for the
// same SessionName as keep before storing a new entry. The transcript
// grows monotonically, so older size-keyed entries for the same
// session are obsolete and should not accumulate. The caller MUST
// hold c.mu.
func (c *contextUsageStateCache) evictOtherSessionEntriesLocked(keep contextUsageCacheKey) {
	for existing := range c.states {
		if existing.SessionName == keep.SessionName && existing != keep {
			delete(c.states, existing)
		}
	}
}

func contextUsageKeyForSession(sess *session.Session) (contextUsageCacheKey, bool) {
	if sess == nil {
		return emptyContextUsageCacheKey(), false
	}
	transcriptPath := strings.TrimSpace(sess.Metadata.ProviderTranscriptPath())
	sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
	if sess.Name == "" || sessionID == "" || transcriptPath == "" {
		return emptyContextUsageCacheKey(), false
	}
	key := contextUsageCacheKey{
		SessionName:         sess.Name,
		Provider:            string(sess.ProviderID()),
		SessionID:           sessionID,
		TranscriptPath:      transcriptPath,
		TranscriptSizeBytes: 0,
	}
	if info, err := os.Stat(transcriptPath); err == nil {
		key.TranscriptSizeBytes = info.Size()
	}
	return key, true
}

func emptySessionContextState() sessionContextState {
	var state sessionContextState
	return state
}

func emptyContextUsageCacheKey() contextUsageCacheKey {
	var key contextUsageCacheKey
	return key
}
