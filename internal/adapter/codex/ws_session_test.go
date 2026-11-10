package codex

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCacheTakeReturnsFalseOnColdCache(t *testing.T) {
	c := NewWebsocketSessionCache(nil, time.Minute, nil)
	if _, ok := c.Take(context.Background(), "conv-1"); ok {
		t.Errorf("expected cold cache Take to return false")
	}
}

func TestCachePutThenTakeReturnsSameSession(t *testing.T) {
	c := NewWebsocketSessionCache(nil, time.Minute, nil)
	s := &WebsocketSession{ConversationID: "conv-1", LastResponseID: "resp-a", OpenedAt: time.Now(), LastUsed: time.Now()}
	c.Put(context.Background(), s)
	got, ok := c.Take(context.Background(), "conv-1")
	if !ok {
		t.Fatalf("expected hit after Put")
	}
	if got != s {
		t.Errorf("expected same pointer, got different")
	}
	if got.LastResponseID != "resp-a" {
		t.Errorf("LastResponseID lost: %q", got.LastResponseID)
	}
}

func TestCacheTakeTwiceWithoutPutReturnsFalseOnSecond(t *testing.T) {
	c := NewWebsocketSessionCache(nil, time.Minute, nil)
	c.Put(context.Background(), &WebsocketSession{ConversationID: "conv-1", OpenedAt: time.Now(), LastUsed: time.Now()})
	if _, ok := c.Take(context.Background(), "conv-1"); !ok {
		t.Fatalf("first Take should succeed")
	}
	if _, ok := c.Take(context.Background(), "conv-1"); ok {
		t.Errorf("second Take without Put should return false (lock-out)")
	}
}

func TestCacheIdleExpirationDropsEntry(t *testing.T) {
	c := NewWebsocketSessionCache(nil, 50*time.Millisecond, nil)
	now := time.Now()
	c.now = func() time.Time { return now }
	c.Put(context.Background(), &WebsocketSession{ConversationID: "conv-1", OpenedAt: now, LastUsed: now})
	c.now = func() time.Time { return now.Add(time.Second) }
	if _, ok := c.Take(context.Background(), "conv-1"); ok {
		t.Errorf("expired entry should not be returned")
	}
	if c.Size() != 0 {
		t.Errorf("expired entry should be evicted, size=%d", c.Size())
	}
}

func TestCacheInvalidateClosesEntry(t *testing.T) {
	c := NewWebsocketSessionCache(nil, time.Minute, nil)
	s := &WebsocketSession{ConversationID: "conv-1", OpenedAt: time.Now(), LastUsed: time.Now()}
	c.Put(context.Background(), s)
	c.Invalidate(context.Background(), "conv-1", "ws_read_error")
	if _, ok := c.Take(context.Background(), "conv-1"); ok {
		t.Errorf("invalidated entry should not be takeable")
	}
	if !s.Closed {
		t.Errorf("entry should be marked closed after Invalidate")
	}
	if s.InvalidationReason != "ws_read_error" {
		t.Errorf("expected reason ws_read_error, got %q", s.InvalidationReason)
	}
}

func TestCacheCloseAllClosesEverything(t *testing.T) {
	c := NewWebsocketSessionCache(nil, time.Minute, nil)
	a := &WebsocketSession{ConversationID: "conv-a", OpenedAt: time.Now(), LastUsed: time.Now()}
	b := &WebsocketSession{ConversationID: "conv-b", OpenedAt: time.Now(), LastUsed: time.Now()}
	c.Put(context.Background(), a)
	c.Put(context.Background(), b)
	c.CloseAll(context.Background(), "shutdown")
	if c.Size() != 0 {
		t.Errorf("expected empty after CloseAll, got %d", c.Size())
	}
	if !a.Closed || !b.Closed {
		t.Errorf("entries should be closed")
	}
	if a.InvalidationReason != "shutdown" || b.InvalidationReason != "shutdown" {
		t.Errorf("reason not propagated")
	}
}

func TestCachePutOnClosedSessionInvalidates(t *testing.T) {
	c := NewWebsocketSessionCache(nil, time.Minute, nil)
	s := &WebsocketSession{ConversationID: "conv-1", Closed: true, OpenedAt: time.Now(), LastUsed: time.Now()}
	c.Put(context.Background(), s)
	if c.Size() != 0 {
		t.Errorf("closed entry should not be cached")
	}
	if s.InvalidationReason != "already_closed_on_put" {
		t.Errorf("expected invalidation reason, got %q", s.InvalidationReason)
	}
}

func TestCacheConcurrentTakePutInvalidateRaceFree(t *testing.T) {
	ctx := context.Background()
	c := NewWebsocketSessionCache(nil, time.Minute, nil)
	c.Put(ctx, &WebsocketSession{ConversationID: "conv-1", OpenedAt: time.Now(), LastUsed: time.Now()})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if s, ok := c.Take(ctx, "conv-1"); ok {
					c.Put(ctx, s)
				}
				_ = c.Size()
				c.Invalidate(ctx, "conv-other", "noop")
			}
		})
	}
	wg.Wait()
}
