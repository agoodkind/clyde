package daemon

import (
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// blankTextKinds selects chat, reasoning, and tool calls together, so a message
// is dropped only when every one of them is empty rather than because a class
// was switched off.
func blankTextKinds(t *testing.T) conversation.ContentKindSet {
	t.Helper()
	return conversation.NewContentKindSet(
		conversation.ContentKindChat,
		conversation.ContentKindThinking,
		conversation.ContentKindToolCalls,
	)
}

// TestWhitespaceOnlyTextIsOfferedAsNoText proves a message whose text holds only
// spacing is offered with no text at all.
//
// The receiving contract carries text as a plain string, where an unset field
// and an empty one are the same bytes, so absence is expressible only as empty.
// A single space is content on the wire and would be stored as a row a search
// can never return, because there is nothing in it to match. Normalizing here
// keeps that decision with the sender, which is the side that knows whether the
// spacing meant anything.
func TestWhitespaceOnlyTextIsOfferedAsNoText(t *testing.T) {
	t.Parallel()

	kinds := blankTextKinds(t)
	for _, spacing := range []string{" ", "   ", "\n", "\t", " \n\t "} {
		messages := []transcript.Message{{
			Role:      "assistant",
			Timestamp: time.Unix(1710000000, 0),
			Text:      spacing,
			HasTools:  true,
			Tools: []transcript.ToolCall{{
				Name:  "Bash",
				Input: transcript.ToolInputJSON{Raw: []byte(`{"command":"ls"}`)},
			}},
		}}
		built, err := BuildSemanticConversationDocuments(policyTestRecord(), messages, kinds)
		if err != nil {
			t.Fatalf("BuildSemanticConversationDocuments(%q) returned error: %v", spacing, err)
		}
		if len(built.Docs) != 1 {
			t.Fatalf("text %q produced %d documents, want 1 for its tool call", spacing, len(built.Docs))
		}
		if built.Docs[0].Text != "" {
			t.Fatalf("text %q was offered as %q, want no text", spacing, built.Docs[0].Text)
		}
	}
}

// TestWhitespaceOnlyTextWithNothingElseIsNotOffered proves such a message is
// dropped entirely when it also carries no reasoning and no tool call, which is
// what the existing filter already does for a message whose text is exactly
// empty.
func TestWhitespaceOnlyTextWithNothingElseIsNotOffered(t *testing.T) {
	t.Parallel()

	messages := []transcript.Message{{
		Role:      "assistant",
		Timestamp: time.Unix(1710000000, 0),
		Text:      "   \n  ",
	}}
	built, err := BuildSemanticConversationDocuments(policyTestRecord(), messages, blankTextKinds(t))
	if err != nil {
		t.Fatalf("BuildSemanticConversationDocuments returned error: %v", err)
	}
	if len(built.Docs) != 0 {
		t.Fatalf("offered %d documents for a message holding only spacing, want none", len(built.Docs))
	}
	if built.PolicySkipped != 1 {
		t.Fatalf("PolicySkipped = %d, want 1", built.PolicySkipped)
	}
}

// TestTextWithContentIsOfferedByteForByte pins that normalization touches only
// text that is entirely spacing.
//
// The receiver compares a delivered text against the stored one to decide
// whether a message changed, so trimming text that has content would make every
// stored message differ from its newly delivered form and re-embed the whole
// collection for no gain. Leading and trailing spacing therefore survives
// exactly as written.
func TestTextWithContentIsOfferedByteForByte(t *testing.T) {
	t.Parallel()

	kinds := blankTextKinds(t)
	for _, text := range []string{
		"  leading spacing is preserved",
		"trailing spacing is preserved  ",
		"\n\nboth ends\n\n",
		"interior  spacing  preserved",
	} {
		messages := []transcript.Message{{
			Role:      "assistant",
			Timestamp: time.Unix(1710000000, 0),
			Text:      text,
		}}
		built, err := BuildSemanticConversationDocuments(policyTestRecord(), messages, kinds)
		if err != nil {
			t.Fatalf("BuildSemanticConversationDocuments(%q) returned error: %v", text, err)
		}
		if len(built.Docs) != 1 {
			t.Fatalf("text %q produced %d documents, want 1", text, len(built.Docs))
		}
		if built.Docs[0].Text != text {
			t.Fatalf("text %q was offered as %q, want it unchanged", text, built.Docs[0].Text)
		}
	}
}
