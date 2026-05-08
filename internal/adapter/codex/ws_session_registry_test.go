package codex

import (
	"context"
	"testing"
	"time"

	"goodkind.io/clyde/internal/livetrack"
)

// callRecordingCloser satisfies [livetrack.Closer] and invokes onClose
// when Close is called, allowing tests to observe force-close events
// without a real websocket connection.
type callRecordingCloser struct {
	onClose func(reason string)
}

func (c *callRecordingCloser) Close(reason string) error {
	if c.onClose != nil {
		c.onClose(reason)
	}
	return nil
}

// TestWsSessionCacheRegistryParity verifies that:
//   - Put registers the session in the livetrack registry.
//   - Invalidate releases the registry entry.
//   - CloseAll with a registry delegates to ForceCloseMatching and
//     yields the same behavioral outcome as the legacy path.
func TestWsSessionCacheRegistryParity(t *testing.T) {
	t.Parallel()
	reg := NewWsSessionRegistry()
	cache := NewWebsocketSessionCache(nil, time.Minute, reg)
	ctx := context.Background()

	// Put a session; the registry should track it.
	s := &WebsocketSession{
		ConversationID: "conv-parity",
		Model:          "gpt-4",
		OpenedAt:       time.Now(),
		LastUsed:       time.Now(),
	}
	cache.Put(ctx, s)
	if reg.Count() != 1 {
		t.Fatalf("expected 1 registry entry after Put, got %d", reg.Count())
	}

	// Invalidate removes from both the in-memory cache and the registry.
	cache.Invalidate(ctx, "conv-parity", "test.invalidate")
	if cache.Size() != 0 {
		t.Errorf("cache should be empty after Invalidate, got %d", cache.Size())
	}
	if reg.Count() != 0 {
		t.Errorf("registry should be empty after Invalidate, got %d", reg.Count())
	}
	if !s.Closed {
		t.Error("session should be marked closed after Invalidate")
	}
}

func TestWsSessionCacheCloseAllUsesRegistry(t *testing.T) {
	t.Parallel()
	reg := NewWsSessionRegistry()
	cache := NewWebsocketSessionCache(nil, time.Minute, reg)
	ctx := context.Background()

	a := &WebsocketSession{ConversationID: "conv-a", OpenedAt: time.Now(), LastUsed: time.Now()}
	b := &WebsocketSession{ConversationID: "conv-b", OpenedAt: time.Now(), LastUsed: time.Now()}
	cache.Put(ctx, a)
	cache.Put(ctx, b)

	if reg.Count() != 2 {
		t.Fatalf("expected 2 registry entries, got %d", reg.Count())
	}

	// CloseAll with registry uses ForceCloseMatching.
	cache.CloseAll(context.Background(), "test.close_all")

	if cache.Size() != 0 {
		t.Errorf("expected empty cache after CloseAll, got %d", cache.Size())
	}
	if reg.Count() != 0 {
		t.Errorf("expected empty registry after CloseAll, got %d", reg.Count())
	}
}

func TestWsSessionCacheCloseAllLegacyPathNoRegistry(t *testing.T) {
	t.Parallel()
	// No registry: legacy path should still close everything.
	cache := NewWebsocketSessionCache(nil, time.Minute, nil)
	ctx := context.Background()

	a := &WebsocketSession{ConversationID: "conv-a", OpenedAt: time.Now(), LastUsed: time.Now()}
	b := &WebsocketSession{ConversationID: "conv-b", OpenedAt: time.Now(), LastUsed: time.Now()}
	cache.Put(ctx, a)
	cache.Put(ctx, b)
	cache.CloseAll(context.Background(), "test.legacy")

	if cache.Size() != 0 {
		t.Errorf("expected empty cache, got %d", cache.Size())
	}
	if !a.Closed || !b.Closed {
		t.Error("entries should be closed by legacy CloseAll")
	}
}

// TestWsSessionRegistryForceCloseMatchingBehavior registers a session
// directly into the ws-session registry using a callRecordingCloser and
// verifies that ForceCloseMatching invokes the closer and removes the
// session from the registry. This tests the migration parity: the
// previous CloseAll-based shutdown now goes through ForceCloseMatching.
func TestWsSessionRegistryForceCloseMatchingBehavior(t *testing.T) {
	t.Parallel()
	reg := NewWsSessionRegistry()

	sess := &WebsocketSession{
		ConversationID: "conv-force",
		OpenedAt:       time.Now(),
		LastUsed:       time.Now(),
	}
	var closeCalled bool
	closer := &callRecordingCloser{onClose: func(_ string) {
		closeCalled = true
		sess.Closed = true
	}}

	liveSess, err := reg.Register(context.Background(), wsSessionKind, WsSessionMeta{
		ConversationID: "conv-force",
		Model:          "",
		FrameCount:     0,
	}, closer)
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if liveSess == nil {
		t.Fatal("expected non-nil session from Register")
	}

	// ForceCloseMatching should call the closer and remove the session.
	allPred := func(livetrack.Session[WsSessionMeta]) bool { return true }
	closedCount := reg.ForceCloseMatching(context.Background(), allPred, "test.force_matching")
	if closedCount != 1 {
		t.Errorf("ForceCloseMatching returned %d, want 1", closedCount)
	}
	if !closeCalled {
		t.Error("closer should have been called by ForceCloseMatching")
	}
	if !sess.Closed {
		t.Error("session should be marked closed after ForceCloseMatching")
	}
	if reg.Count() != 0 {
		t.Errorf("registry should be empty after ForceCloseMatching, got %d", reg.Count())
	}
}

// TestWsConnCloserNilConnIsNoop verifies that wsConnCloser.Close is a
// no-op when the underlying websocket.Conn is nil, preventing panics
// on half-constructed sessions.
func TestWsConnCloserNilConnIsNoop(t *testing.T) {
	t.Parallel()
	sess := &WebsocketSession{ConversationID: "conv-nil", OpenedAt: time.Now()}
	closer := &wsConnCloser{session: sess}
	if err := closer.Close("test"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Nil conn: session.Closed stays false because nothing was closed.
	if sess.Closed {
		t.Error("nil-conn Close should not mark session closed")
	}
}
