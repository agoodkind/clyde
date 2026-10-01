package daemon

import (
	"testing"
)

// TestProjectionHashSeparatesContentFromBoundaries pins the hash contract:
// identical projections hash equal, any content difference hashes differently,
// and field boundaries cannot collide.
func TestProjectionHashSeparatesContentFromBoundaries(t *testing.T) {
	t.Parallel()
	base := []SemanticDocument{{
		ConversationID: "c",
		MessageIndex:   0,
		Role:           "user",
		Text:           "ab",
		Thinking:       "cd",
	}}
	same := []SemanticDocument{{
		ConversationID: "c",
		MessageIndex:   0,
		Role:           "user",
		Text:           "ab",
		Thinking:       "cd",
	}}
	shifted := []SemanticDocument{{
		ConversationID: "c",
		MessageIndex:   0,
		Role:           "user",
		Text:           "abc",
		Thinking:       "d",
	}}
	if SemanticProjectionHash(base) != SemanticProjectionHash(same) {
		t.Fatal("identical projections hash differently")
	}
	if SemanticProjectionHash(base) == SemanticProjectionHash(shifted) {
		t.Fatal("boundary-shifted projection hashes equal, want a distinct hash")
	}
}
