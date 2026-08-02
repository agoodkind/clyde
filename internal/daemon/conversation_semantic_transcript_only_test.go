package daemon

import (
	"testing"
	"time"

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

// TestBuildSumsHarnessStripCounts proves the projection carries the parsers'
// strip counts through to the caller, so a pass log can state that stripping
// ran without the generic layer knowing any provider's markers.
func TestBuildSumsHarnessStripCounts(t *testing.T) {
	t.Parallel()

	kinds := blankTextKinds(t)
	messages := []transcript.Message{
		{
			Role:          "user",
			Visibility:    transcript.MessageVisibilityVisible,
			Timestamp:     time.Unix(1710000000, 0),
			Text:          "typed prompt",
			HarnessStrips: transcript.HarnessStrips{Injected: 2, System: 1},
		},
		{
			Role:          "user",
			Visibility:    transcript.MessageVisibilityVisible,
			Timestamp:     time.Unix(1710000060, 0),
			Text:          "second prompt",
			HarnessStrips: transcript.HarnessStrips{Injected: 1, System: 0},
		},
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), messages, kinds)
	if err != nil {
		t.Fatalf("BuildSemanticConversationDocuments returned error: %v", err)
	}
	if built.InjectedStripped != 3 {
		t.Fatalf("InjectedStripped = %d, want 3", built.InjectedStripped)
	}
	if built.SystemStripped != 1 {
		t.Fatalf("SystemStripped = %d, want 1", built.SystemStripped)
	}
}
