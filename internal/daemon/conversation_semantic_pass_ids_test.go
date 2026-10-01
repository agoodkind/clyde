package daemon

import (
	"testing"
)

// TestBoundedConversationIDsCapsTheLogLine pins the bound: a backlog pass
// delivering hundreds of conversations logs only the batch head, and the
// existing sent_conversations count carries the total.
func TestBoundedConversationIDsCapsTheLogLine(t *testing.T) {
	t.Parallel()
	ids := make([]string, 0, maxLoggedConversationIDs+5)
	for i := 0; i < maxLoggedConversationIDs+5; i++ {
		ids = append(ids, string(rune('a'+i)))
	}
	bounded := boundedConversationIDs(ids)
	if len(bounded) != maxLoggedConversationIDs {
		t.Fatalf("bounded length = %d, want %d", len(bounded), maxLoggedConversationIDs)
	}
	if bounded[0] != ids[0] {
		t.Fatalf("bounded head = %q, want the delivery-order head %q", bounded[0], ids[0])
	}
	short := []string{"one", "two"}
	if got := boundedConversationIDs(short); len(got) != 2 {
		t.Fatalf("short list length = %d, want 2 unchanged", len(got))
	}
}
