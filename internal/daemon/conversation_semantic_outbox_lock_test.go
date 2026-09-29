package daemon

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestConversationSemanticOutboxOpensOnce opens the outbox of one pool and
// opens it again while the first outbox is open, the way a replacement daemon
// worker opens it during a reload before the old worker drains. The second
// open must fail with errOutboxOwned. After the first outbox closes, a new
// open must succeed.
func TestConversationSemanticOutboxOpensOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	path := conversationSemanticOutboxPath("lock-test")
	first, err := openConversationSemanticOutbox(t.Context(), path)
	if err != nil {
		t.Fatalf("open first outbox: %v", err)
	}
	second, err := openConversationSemanticOutbox(t.Context(), path)
	if err == nil {
		_ = second.Close()
		_ = first.Close()
		t.Fatal("second outbox opened while the first outbox was open")
	}
	if !errors.Is(err, errOutboxOwned) {
		_ = first.Close()
		t.Fatalf("second open error = %v, want errOutboxOwned", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first outbox: %v", err)
	}
	third, err := openConversationSemanticOutbox(t.Context(), path)
	if err != nil {
		t.Fatalf("open outbox after close: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("close outbox: %v", err)
	}
}
