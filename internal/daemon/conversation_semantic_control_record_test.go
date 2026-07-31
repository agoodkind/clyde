package daemon

import (
	"testing"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

// TestControlRecordsAreNotOffered proves a message a provider marked as a
// control record is withheld from the feed.
//
// A harness writes prompts of its own into a conversation: Cursor asks its
// model to summarize a finished subagent, and Claude Code records entries it
// flagged itself. Embedding one costs the same work as a real message and
// returns a row nobody searched for, and the same prompt repeats across a
// conversation, so the cost is paid again per copy. Reading already withholds
// these, and this keeps the feed and the reader saying the same thing about
// what counts as conversation.
func TestControlRecordsAreNotOffered(t *testing.T) {
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
			Visibility: transcript.MessageVisibilityMetaOnly,
			Timestamp:  time.Unix(1710000060, 0),
			Text:       "Briefly inform the user about the task result and perform any follow-up actions (if needed).",
		},
		{
			Role:       "assistant",
			Visibility: transcript.MessageVisibilityVisible,
			Timestamp:  time.Unix(1710000120, 0),
			Text:       "the lockfile is stale",
		},
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), messages, kinds)
	if err != nil {
		t.Fatalf("BuildSemanticConversationDocuments returned error: %v", err)
	}
	if len(built.Docs) != 2 {
		t.Fatalf("documents = %d, want 2: the control record must not be offered", len(built.Docs))
	}
	if built.PolicySkipped != 1 {
		t.Fatalf("PolicySkipped = %d, want 1 (the control record)", built.PolicySkipped)
	}
	for _, doc := range built.Docs {
		if doc.Text == messages[1].Text {
			t.Fatalf("the control record reached the feed as %q", doc.Text)
		}
	}
	// The surviving documents keep the indices they had in the conversation,
	// so a control record between two turns does not renumber what follows and
	// re-embed messages whose content never changed.
	if built.Docs[0].MessageIndex != 0 || built.Docs[1].MessageIndex != 2 {
		t.Fatalf("message indices = %d and %d, want 0 and 2",
			built.Docs[0].MessageIndex, built.Docs[1].MessageIndex)
	}
}
