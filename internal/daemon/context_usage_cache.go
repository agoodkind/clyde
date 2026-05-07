package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/session"
)

var errContextUsageSessionUnavailable = errors.New("context usage session unavailable")

type contextUsageProbeFunc func(context.Context, *session.Session) (sessionContextState, error)

type contextUsageCacheKey struct {
	SessionName            string
	Provider               string
	SessionID              string
	TranscriptPath         string
	TranscriptModUnixNanos int64
	TranscriptSizeBytes    int64
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

type contextUsageStateCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	states   map[contextUsageCacheKey]contextUsageCacheEntry
	inflight map[contextUsageCacheKey]*contextUsageInflight
}

func newContextUsageStateCache(ttl time.Duration) *contextUsageStateCache {
	cache := new(contextUsageStateCache)
	cache.ttl = ttl
	cache.states = make(map[contextUsageCacheKey]contextUsageCacheEntry)
	cache.inflight = make(map[contextUsageCacheKey]*contextUsageInflight)
	return cache
}

func (c *contextUsageStateCache) Get(sess *session.Session, now time.Time) (sessionContextState, bool) {
	if c == nil {
		return emptySessionContextState(), false
	}
	key, ok := contextUsageKeyForSession(sess)
	if !ok {
		return emptySessionContextState(), false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(key, now)
}

func (c *contextUsageStateCache) Refresh(ctx context.Context, sess *session.Session, now time.Time, probe contextUsageProbeFunc) (sessionContextState, error) {
	key, ok := contextUsageKeyForSession(sess)
	if !ok {
		return emptySessionContextState(), errContextUsageSessionUnavailable
	}
	c.mu.Lock()
	if state, hit := c.getLocked(key, now); hit {
		c.mu.Unlock()
		return state, nil
	}
	if running := c.inflight[key]; running != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return emptySessionContextState(), fmt.Errorf("wait context usage refresh: %w", ctx.Err())
		case <-running.Done:
			return running.State, running.Err
		}
	}
	running := &contextUsageInflight{
		Done:  make(chan struct{}),
		State: emptySessionContextState(),
		Err:   nil,
	}
	c.inflight[key] = running
	c.mu.Unlock()

	state, err := probe(ctx, sess)
	if err == nil {
		state.Loaded = true
		state.Refreshing = false
		state.Status = ""
	}

	c.mu.Lock()
	if err == nil {
		c.states[key] = contextUsageCacheEntry{
			State:    state,
			CachedAt: now,
		}
	}
	running.State = state
	running.Err = err
	delete(c.inflight, key)
	close(running.Done)
	c.mu.Unlock()
	return state, err
}

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
}

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
}

func (c *contextUsageStateCache) getLocked(key contextUsageCacheKey, now time.Time) (sessionContextState, bool) {
	entry, ok := c.states[key]
	if !ok {
		return emptySessionContextState(), false
	}
	if c.ttl > 0 && now.Sub(entry.CachedAt) > c.ttl {
		delete(c.states, key)
		return emptySessionContextState(), false
	}
	return entry.State, true
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
		SessionName:            sess.Name,
		Provider:               string(sess.ProviderID()),
		SessionID:              sessionID,
		TranscriptPath:         transcriptPath,
		TranscriptModUnixNanos: 0,
		TranscriptSizeBytes:    0,
	}
	if info, err := os.Stat(transcriptPath); err == nil {
		key.TranscriptModUnixNanos = info.ModTime().UnixNano()
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
