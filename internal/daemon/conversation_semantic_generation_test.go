package daemon

import (
	"context"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// cursorTranscriptRecord is a Cursor JSONL transcript record, whose fingerprint
// carries the fixed compatibility suffix.
func cursorTranscriptRecord(conversationID string) conversation.Record {
	record := semanticTestRecord(conversationID)
	record.Provider = conversation.ProviderCursor
	record.ArtifactKind = string(conversation.ArtifactKindCursorAgentTranscript)
	return record
}

func TestManifestPreservesTheCursorCompatibilitySuffix(t *testing.T) {
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
	if advertised != stamp.Fingerprint()+":r1" {
		t.Fatalf("advertised fingerprint = %q, want the fixed Cursor suffix", advertised)
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
	// The only conversation is suppressed, so the manifest is empty. The pass
	// still syncs that empty manifest, because the engine's copy of the
	// manifest is whatever the last sync stated; what must not happen is the
	// suppressed conversation being advertised again.
	if len(client.syncCalls) != 2 {
		t.Fatalf("sync calls = %d, want 2; the suppressing pass still states the empty manifest", len(client.syncCalls))
	}
	if len(client.syncCalls[1].Manifest) != 0 {
		t.Fatalf("second manifest = %+v, want empty; the suppressed conversation must not be re-advertised", client.syncCalls[1].Manifest)
	}
}
