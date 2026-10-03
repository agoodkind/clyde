package daemon

import (
	"context"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// pinningTestMessages is the unchanged transcript body the touch simulation
// replays across stamps.
func pinningTestMessages() []transcript.Message {
	return []transcript.Message{
		{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "alpha"},
		{Role: "assistant", Timestamp: time.Unix(1710000030, 0), Text: "beta"},
	}
}

// TestHourlyTouchWithoutChangeIsPinnedNotRedelivered replays the CLYDE-640
// shape: an artifact whose mtime moves with identical bytes. Pass one delivers
// and records the memo. Pass two sees a new stamp, projects identical bytes,
// delivers nothing, and counts the pin. Pass three advertises the checkpointed
// fingerprint, so the engine reports nothing needed and the cycle is dead.
func TestHourlyTouchWithoutChangeIsPinnedNotRedelivered(t *testing.T) {
	conversationID := "codex:hourly-touch"
	record := semanticTestRecord(conversationID)
	firstStamp := semanticTestStamp(20, 200)
	index := &fakeConversationSemanticIndex{
		records:      []conversation.StampedRecord{{Record: record, Stamp: firstStamp}},
		messagesByID: map[string][]transcript.Message{conversationID: pinningTestMessages()},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("pass one returned error: %v", err)
	}
	if len(client.upsertCalls) != 1 {
		t.Fatalf("pass one upserts = %d, want 1", len(client.upsertCalls))
	}
	firstFingerprint := conversation.ContentFingerprint(record, firstStamp)

	// The hourly toucher: same bytes, moved stamp. The engine's checkpoint
	// still holds the first fingerprint, so it lists the conversation needed.
	touchedStamp := semanticTestStamp(20, 3800)
	index.records = []conversation.StampedRecord{{Record: record, Stamp: touchedStamp}}
	worker.activeJobID = ""
	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("pass two returned error: %v", err)
	}
	if len(client.upsertCalls) != 1 {
		t.Fatalf("pass two upserts = %d, want 1: unchanged bytes must not re-deliver", len(client.upsertCalls))
	}
	pin, pinned := worker.pinnedFingerprints[conversationID]
	if !pinned {
		t.Fatal("pass two recorded no fingerprint pin for the touched conversation")
	}
	if pin.advertise != firstFingerprint {
		t.Fatalf("pin advertises %q, want the checkpointed fingerprint %q", pin.advertise, firstFingerprint)
	}
	if pin.observed != conversation.ContentFingerprint(record, touchedStamp) {
		t.Fatalf("pin observed %q, want the touched fingerprint", pin.observed)
	}

	// Pass three: the manifest must advertise the checkpointed fingerprint for
	// the touched stamp, so the engine sees no drift.
	client.needed = nil
	worker.activeJobID = ""
	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("pass three returned error: %v", err)
	}
	lastSync := client.syncCalls[len(client.syncCalls)-1]
	if len(lastSync.Manifest) != 1 {
		t.Fatalf("pass three manifest size = %d, want 1", len(lastSync.Manifest))
	}
	if lastSync.Manifest[0].Value != firstFingerprint {
		t.Fatalf("pass three advertises %q, want the pinned checkpointed fingerprint %q", lastSync.Manifest[0].Value, firstFingerprint)
	}
}

// TestRealContentChangeBreaksThePin proves the pin never hides a real edit: a
// stamp the pin has not verified drops the pin, the manifest advertises the
// true fingerprint, and changed bytes deliver.
func TestRealContentChangeBreaksThePin(t *testing.T) {
	conversationID := "codex:pin-break"
	record := semanticTestRecord(conversationID)
	index := &fakeConversationSemanticIndex{
		records:      []conversation.StampedRecord{{Record: record, Stamp: semanticTestStamp(20, 200)}},
		messagesByID: map[string][]transcript.Message{conversationID: pinningTestMessages()},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())
	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("seed pass returned error: %v", err)
	}

	// Touch without change: pin forms.
	index.records = []conversation.StampedRecord{{Record: record, Stamp: semanticTestStamp(20, 3800)}}
	worker.activeJobID = ""
	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("touch pass returned error: %v", err)
	}
	if _, pinned := worker.pinnedFingerprints[conversationID]; !pinned {
		t.Fatal("touch pass formed no pin")
	}

	// A real edit: new stamp and new bytes. The pin must drop, the manifest
	// must advertise the true new fingerprint, and the content must deliver.
	editedStamp := semanticTestStamp(40, 7400)
	index.records = []conversation.StampedRecord{{Record: record, Stamp: editedStamp}}
	index.messagesByID[conversationID] = append(pinningTestMessages(),
		transcript.Message{Role: "user", Timestamp: time.Unix(1710007400, 0), Text: "gamma"})
	client.needed = []string{conversationID}
	worker.activeJobID = ""
	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("edit pass returned error: %v", err)
	}
	if _, pinned := worker.pinnedFingerprints[conversationID]; pinned {
		t.Fatal("pin survived a real content change")
	}
	lastSync := client.syncCalls[len(client.syncCalls)-1]
	if lastSync.Manifest[0].Value != conversation.ContentFingerprint(record, editedStamp) {
		t.Fatalf("edit pass advertises %q, want the true fingerprint", lastSync.Manifest[0].Value)
	}
	if len(client.upsertCalls) != 2 {
		t.Fatalf("upserts = %d, want 2: the edited content must deliver", len(client.upsertCalls))
	}
	lastUpsert := client.upsertCalls[len(client.upsertCalls)-1]
	if len(lastUpsert.Docs) != 3 {
		t.Fatalf("edit delivery docs = %d, want 3", len(lastUpsert.Docs))
	}
}
