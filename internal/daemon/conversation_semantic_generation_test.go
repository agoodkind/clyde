package daemon

import (
	"context"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// cursorTranscriptRecord is a Cursor JSONL transcript record, the artifact kind
// whose reader generation is bumped. The existing manifest test uses a Codex
// record, which carries no generation, so it cannot see this wiring at all.
func cursorTranscriptRecord(conversationID string) conversation.Record {
	record := semanticTestRecord(conversationID)
	record.Provider = conversation.ProviderCursor
	record.ArtifactKind = string(conversation.ArtifactKindCursorAgentTranscript)
	return record
}

// TestManifestAdvertisesTheReaderGeneration covers the line that wires the
// generation into the manifest. Without it the daemon advertises the bare stamp,
// the engine keeps the indexes built by the old reader, and every stored Cursor
// message index goes on pointing at a different turn.
func TestManifestAdvertisesTheReaderGeneration(t *testing.T) {
	conversationID := "cursor:generation"
	stamp := semanticTestStamp(20, 200)
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: cursorTranscriptRecord(conversationID), Stamp: stamp}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "first"}},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}
	if len(client.syncCalls) != 1 || len(client.syncCalls[0].Manifest) != 1 {
		t.Fatalf("sync calls = %+v, want one manifest entry", client.syncCalls)
	}

	advertised := client.syncCalls[0].Manifest[0].Value
	want := conversation.ContentFingerprint(cursorTranscriptRecord(conversationID), stamp)
	if advertised != want {
		t.Fatalf("advertised fingerprint = %q, want %q", advertised, want)
	}
	if advertised == stamp.Fingerprint() {
		t.Fatalf("advertised fingerprint = %q, the bare stamp; the reader generation is not wired in", advertised)
	}
}

// TestZeroDocumentSuppressionUsesTheAdvertisedFingerprint covers the second
// wiring line. The suppression map must hold the same value the manifest states,
// or a conversation suppressed under one fingerprint is compared against another
// and the suppression never lifts, or lifts every pass.
func TestZeroDocumentSuppressionUsesTheAdvertisedFingerprint(t *testing.T) {
	conversationID := "cursor:empty"
	stamp := semanticTestStamp(20, 200)
	index := &fakeConversationSemanticIndex{
		records:      []conversation.StampedRecord{{Record: cursorTranscriptRecord(conversationID), Stamp: stamp}},
		messagesByID: map[string][]transcript.Message{conversationID: {}},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("first runPass returned error: %v", err)
	}
	want := conversation.ContentFingerprint(cursorTranscriptRecord(conversationID), stamp)
	if got := worker.emptyDelivered[conversationID]; got != want {
		t.Fatalf("suppression fingerprint = %q, want the advertised %q", got, want)
	}

	// Nothing changed, so the next manifest omits it rather than re-offering a
	// conversation the engine can never mark satisfied.
	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("second runPass returned error: %v", err)
	}
	// The only conversation is suppressed, so the manifest is empty and the pass
	// returns before it reaches the engine at all.
	if len(client.syncCalls) != 1 {
		t.Fatalf("sync calls = %d, want 1; the suppressed conversation must not be re-advertised", len(client.syncCalls))
	}
}
