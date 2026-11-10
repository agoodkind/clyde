package codex

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"goodkind.io/clyde/codexwire"
	"goodkind.io/clyde/internal/livetrack"
)

// WebsocketSession holds the state codex CLI and Codex Desktop both
// hold per conversation. Reference: research/codex/codex-rs/core/src/
// client.rs::WebsocketSession. We keep the live websocket connection,
// the most recent server-issued response_id (for chaining via
// previous_response_id), and a baseline of input items so the next
// turn can compute a delta.
type WebsocketSession struct {
	Conn               *websocket.Conn
	ConversationID     string
	Model              string
	PromptCacheKey     string
	LastResponseID     string
	LastInputItems     []codexwire.InputItem
	OpenedAt           time.Time
	LastUsed           time.Time
	FrameCount         int
	Closed             bool
	InvalidationReason string
}

// WebsocketSessionCache holds at most one WebsocketSession per
// conversation_id. Take/Put serializes concurrent requests on the
// same conversation. The lifecycle contract:
//
//   - Take removes the entry from the cache and returns it. A second
//     Take before Put returns (nil, false) so concurrent callers do
//     not share a single websocket frame stream.
//   - Put returns the entry to the cache. LastUsed is bumped to now.
//   - Invalidate closes the connection synchronously and drops the
//     entry. Subsequent Take returns (nil, false).
//   - Idle expiry: Take returns (nil, false) and closes the entry if
//     LastUsed plus idleTTL is in the past.
//   - CloseAll closes every entry. It is preserved as a best-effort
//     fallback; callers should prefer draining wsRegistry so
//     force-close is bounded.
type WebsocketSessionCache struct {
	mu      sync.Mutex
	entries map[string]*WebsocketSession
	// liveSessions maps conversation_id -> livetrack session so the
	// cache can release the registry entry when a session is closed or
	// displaced. The registry owns lifetime tracking; entries is the
	// fast-path lookup for the websocket connection.
	liveSessions map[string]*livetrack.Session[WsSessionMeta]
	log          *slog.Logger
	idleTTL      time.Duration
	now          func() time.Time
	wsRegistry   *livetrack.Registry[WsSessionMeta]
}

// NewWebsocketSessionCache constructs a cache with the given idle
// TTL. The log argument is used by Invalidate and per-action telemetry.
// reg is the per-daemon livetrack registry for Codex websocket sessions.
// When reg is nil the cache falls back to the legacy behavior and
// CloseAll is the only shutdown mechanism.
func NewWebsocketSessionCache(log *slog.Logger, idleTTL time.Duration, reg *livetrack.Registry[WsSessionMeta]) *WebsocketSessionCache {
	return &WebsocketSessionCache{
		entries:      map[string]*WebsocketSession{},
		liveSessions: map[string]*livetrack.Session[WsSessionMeta]{},
		log:          log,
		idleTTL:      idleTTL,
		now:          time.Now,
		wsRegistry:   reg, mu: sync.
				Mutex{},
	}
}

// Take removes the entry for conversationID and returns it. If the
// entry is missing, expired, or closed, returns (nil, false). Stale
// entries are closed before returning false. ctx is used for livetrack
// registry release events.
func (c *WebsocketSessionCache) Take(ctx context.Context, conversationID string) (*WebsocketSession, bool) {
	conv := strings.TrimSpace(conversationID)
	if conv == "" {
		return nil, false
	}
	c.mu.Lock()
	entry, ok := c.entries[conv]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	delete(c.entries, conv)
	// Leave the livetrack session entry in place; the websocket conn
	// is still open while the caller holds the session. The entry is
	// released when the caller puts it back and it gets invalidated,
	// or when force-close fires.
	c.mu.Unlock()
	if entry.Closed {
		c.releaseRegistryEntry(ctx, conv, "already_closed_on_take")
		return nil, false
	}
	if c.idleTTL > 0 && c.now().Sub(entry.LastUsed) > c.idleTTL {
		c.invalidateEntry(entry, "idle_timeout")
		c.releaseRegistryEntry(ctx, conv, "idle_timeout")
		return nil, false
	}
	return entry, true
}

// Put returns the entry to the cache. Subsequent Take with the
// matching conversationID will retrieve it. Calls Invalidate when
// the entry is already marked closed. ctx is used for livetrack
// registry release events.
func (c *WebsocketSessionCache) Put(ctx context.Context, s *WebsocketSession) {
	if s == nil {
		return
	}
	conv := strings.TrimSpace(s.ConversationID)
	if conv == "" {
		c.invalidateEntry(s, "missing_conversation_id")
		return
	}
	if s.Closed {
		c.invalidateEntry(s, "already_closed_on_put")
		return
	}
	s.LastUsed = c.now()
	c.mu.Lock()
	existing, hasExisting := c.entries[conv]
	if hasExisting && existing != s {
		c.mu.Unlock()
		c.invalidateEntry(existing, "displaced_by_put")
		c.releaseRegistryEntry(ctx, conv, "displaced_by_put")
		c.mu.Lock()
	}
	c.entries[conv] = s
	// Register with the livetrack registry when this is a fresh Put
	// (not an update of an already-registered session). The registry
	// assigns a closer that closes the underlying websocket conn so
	// force-close under drain deadline terminates any blocked reader.
	_, alreadyRegistered := c.liveSessions[conv]
	c.mu.Unlock()
	if !alreadyRegistered {
		c.registerEntry(ctx, s)
	}
}

// Invalidate closes any cached entry for conversationID. The reason
// is recorded on the entry and emitted via telemetry. Safe to call
// when the entry is absent. ctx is used for livetrack registry release
// events.
func (c *WebsocketSessionCache) Invalidate(ctx context.Context, conversationID, reason string) {
	conv := strings.TrimSpace(conversationID)
	if conv == "" {
		return
	}
	c.mu.Lock()
	entry, ok := c.entries[conv]
	if ok {
		delete(c.entries, conv)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	c.invalidateEntry(entry, reason)
	c.releaseRegistryEntry(ctx, conv, reason)
}

// CloseAll closes every cached entry. ctx is used for livetrack
// ForceCloseMatching. It is the best-effort fallback when wsRegistry
// is nil or Drain has not been wired. Prefer draining wsRegistry
// directly so force-close is bounded by the registry grace.
func (c *WebsocketSessionCache) CloseAll(ctx context.Context, reason string) {
	if c.wsRegistry != nil {
		allPred := func(livetrack.Session[WsSessionMeta]) bool { return true }
		c.wsRegistry.ForceCloseMatching(ctx, allPred, reason)
		c.mu.Lock()
		c.entries = map[string]*WebsocketSession{}
		c.liveSessions = map[string]*livetrack.Session[WsSessionMeta]{}
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	entries := c.entries
	c.entries = map[string]*WebsocketSession{}
	c.mu.Unlock()
	for _, entry := range entries {
		c.invalidateEntry(entry, reason)
	}
}

// Size returns the number of cached entries. Used by telemetry to
// surface cache depth on each ws_session log event.
func (c *WebsocketSessionCache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// registerEntry adds a live websocket session to the livetrack
// registry. ctx is threaded to the registry for correlation. The
// closer closes the underlying websocket conn. No-op when wsRegistry
// is nil.
func (c *WebsocketSessionCache) registerEntry(ctx context.Context, s *WebsocketSession) {
	if c.wsRegistry == nil {
		return
	}
	conv := strings.TrimSpace(s.ConversationID)
	liveSess, err := c.wsRegistry.Register(
		ctx,
		wsSessionKind,
		WsSessionMeta{
			ConversationID: conv,
			Model:          s.Model,
			FrameCount:     s.FrameCount,
		},
		&wsConnCloser{session: s, conn: s.Conn},
	)
	if err != nil {
		// Registry is draining; log and continue without registration.
		if c.log != nil {
			c.log.WarnContext(ctx, "adapter.codex.ws_session.register_rejected", "concern", "adapter.providers.codex.request", "component", "adapter",
				"subcomponent", "codex",
				"conversation_id", conv,
				"err", err,
			)
		}
		return
	}
	c.mu.Lock()
	c.liveSessions[conv] = liveSess
	c.mu.Unlock()
}

// releaseRegistryEntry releases the livetrack session for conv if one
// exists. No-op when wsRegistry is nil or conv has no registration.
func (c *WebsocketSessionCache) releaseRegistryEntry(ctx context.Context, conv, reason string) {
	if c.wsRegistry == nil {
		return
	}
	c.mu.Lock()
	liveSess, ok := c.liveSessions[conv]
	if ok {
		delete(c.liveSessions, conv)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	c.wsRegistry.Release(ctx, liveSess, reason)
}

func (c *WebsocketSessionCache) invalidateEntry(entry *WebsocketSession, reason string) {
	if entry == nil {
		return
	}
	entry.Closed = true
	entry.InvalidationReason = reason
	if entry.Conn != nil {
		_ = entry.Conn.Close()
	}
	if c.log != nil {
		c.log.Info("adapter.codex.ws_session.invalidated", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"conversation_id", entry.ConversationID,
			"reason", reason,
			"frame_count", entry.FrameCount,
			"last_response_id", entry.LastResponseID,
			"age_ms", c.now().Sub(entry.OpenedAt).Milliseconds(),
		)
	}
}
