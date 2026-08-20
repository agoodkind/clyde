package hookspec

import (
	"context"
	"testing"
)

func TestFileSnapshotStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := NewFileSnapshotStore(t.TempDir())
	key := ReorientSnapshotKey{
		TranscriptPath: "/tmp/session.jsonl",
		SessionID:      "session-1",
		CWD:            "/tmp/project",
	}
	if err := store.Save(context.Background(), key, "saved body"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Consume(context.Background(), key)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !ok || got != "saved body" {
		t.Fatalf("Consume = (%q, %t), want (saved body, true)", got, ok)
	}
	got, ok, err = store.Consume(context.Background(), key)
	if err != nil {
		t.Fatalf("second Consume: %v", err)
	}
	if ok || got != "" {
		t.Fatalf("second Consume = (%q, %t), want missing snapshot", got, ok)
	}
}

func TestSnapshotKeyNormalizesTranscriptPathAndConversationID(t *testing.T) {
	t.Parallel()

	fromTranscript := normalizeSnapshotKey(ReorientSnapshotKey{
		TranscriptPath: "/tmp/session.jsonl",
		SessionID:      "session-1",
		CWD:            "/tmp/project",
	})
	fromConversation := normalizeSnapshotKey(ReorientSnapshotKey{
		ConversationID: "/tmp/session.jsonl",
		SessionID:      "session-1",
		CWD:            "/tmp/project",
	})
	if fromTranscript != fromConversation {
		t.Fatalf("normalized keys differ: %#v != %#v", fromTranscript, fromConversation)
	}
}
