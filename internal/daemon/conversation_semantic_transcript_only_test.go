package daemon

import (
	"context"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// TestTranscriptOnlyRecordsAreNotOffered proves a message the provider marked
// transcript-only is withheld from the feed. Claude marks compact-summary
// records this way, and each one quotes an entire earlier session, so
// embedding one duplicates the whole conversation into rows nobody searched
// for.
func TestTranscriptOnlyRecordsAreNotOffered(t *testing.T) {
	t.Parallel()

	kinds := blankTextKinds(t)
	messages := []transcript.Message{
		{
			Role:       "user",
			Visibility: transcript.MessageVisibilityVisible,
			Timestamp:  time.Unix(1710000000, 0),
			Text:       "why is the build failing?",
		},
		{
			Role:       "user",
			Visibility: transcript.MessageVisibilityTranscriptOnly,
			Timestamp:  time.Unix(1710000060, 0),
			Text:       "This session is being continued from a previous conversation. Summary: the whole prior session quoted here.",
		},
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), messages, kinds)
	if err != nil {
		t.Fatalf("BuildSemanticConversationDocuments returned error: %v", err)
	}
	if len(built.Docs) != 1 {
		t.Fatalf("documents = %d, want 1: the transcript-only record must not be offered", len(built.Docs))
	}
	if built.PolicySkipped != 1 {
		t.Fatalf("PolicySkipped = %d, want 1 (the transcript-only record)", built.PolicySkipped)
	}
	if built.Docs[0].Text != messages[0].Text {
		t.Fatalf("offered text = %q, want the visible message", built.Docs[0].Text)
	}
}

// TestLoadDocsReportsTheLoadTally proves the feeder attaches a tally to the
// load and reports it on the projection, so a pass log states what the parsers
// removed or withheld, including records dropped entirely, without the generic
// layer knowing any provider's markers.
func TestLoadDocsReportsTheLoadTally(t *testing.T) {
	t.Parallel()

	conversationID := "codex:tally-report"
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: semanticTestRecord(conversationID), Stamp: semanticTestStamp(20, 200)}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {
				{Role: "user", Visibility: transcript.MessageVisibilityVisible, Timestamp: time.Unix(1710000000, 0), Text: "typed prompt"},
			},
		},
		loadOptions: nil,
		tally:       transcript.HarnessStrips{Injected: 3, System: 2},
	}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(&fakeConversationSemanticClient{needed: []string{conversationID}}), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	built, err := worker.loadDocs(context.Background(), conversation.Record{ID: conversationID, Provider: providerid.ProviderCodex})
	if err != nil {
		t.Fatalf("loadDocs returned error: %v", err)
	}
	if built.InjectedStripped != 3 {
		t.Fatalf("InjectedStripped = %d, want the tally's 3", built.InjectedStripped)
	}
	if built.SystemStripped != 2 {
		t.Fatalf("SystemStripped = %d, want the tally's 2", built.SystemStripped)
	}
}
