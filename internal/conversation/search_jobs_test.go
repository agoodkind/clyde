package conversation

import (
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/transcript"
)

func withinTestMessages() []transcript.Message {
	messages := make([]transcript.Message, 0, 6)
	texts := []string{"alpha needle", "beta", "gamma needle", "delta", "epsilon needle", "zeta"}
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages = append(messages, transcript.Message{
			Role:      role,
			Timestamp: time.Unix(int64(1000+i), 0),
			Text:      text,
		})
	}
	return messages
}

func emptyWithinOptions() WithinSearchOptions {
	return WithinSearchOptions{Limit: 0, Roles: nil, FromUnix: 0, UntilUnix: 0, MinScore: 0}
}

// TestMergeWithinHitsSemanticFirstThenLiteral proves the merge order: engine
// hits lead in relevance order, the literal scan fills only when the index
// does not cover the transcript, duplicates collapse on message index, and
// the limit bounds the total.
func TestMergeWithinHitsSemanticFirstThenLiteral(t *testing.T) {
	t.Parallel()

	messages := withinTestMessages()
	semanticHits := []semsearch.SemHit{
		{ConversationID: "c", MessageIndex: 4, Role: "user", TimestampUnix: 1004, Content: "epsilon needle", Score: 0.9, ParentConversationID: ""},
		{ConversationID: "c", MessageIndex: 0, Role: "user", TimestampUnix: 1000, Content: "alpha needle", Score: 0.7, ParentConversationID: ""},
		{ConversationID: "c", MessageIndex: 99, Role: "user", TimestampUnix: 0, Content: "out of range", Score: 0.6, ParentConversationID: ""},
	}

	merged := mergeWithinHits(semanticHits, messages, "needle", emptyWithinOptions(), 10, true)
	if len(merged) != 3 {
		t.Fatalf("merged hits = %d, want 3 (two semantic, one literal)", len(merged))
	}
	if merged[0].MessageIndex != 4 || merged[0].Source != hitSourceSemantic || merged[0].Score != 0.9 {
		t.Fatalf("first hit = %+v, want semantic #4 score 0.9", merged[0])
	}
	if merged[1].MessageIndex != 0 || merged[1].Source != hitSourceSemantic {
		t.Fatalf("second hit = %+v, want semantic #0", merged[1])
	}
	if merged[2].MessageIndex != 2 || merged[2].Source != hitSourceLiteral {
		t.Fatalf("third hit = %+v, want literal #2 (deduped #0 and #4)", merged[2])
	}

	fresh := mergeWithinHits(semanticHits, messages, "needle", emptyWithinOptions(), 10, false)
	if len(fresh) != 2 {
		t.Fatalf("fresh-index merge = %d hits, want 2 (no literal fill)", len(fresh))
	}

	bounded := mergeWithinHits(nil, messages, "needle", emptyWithinOptions(), 2, true)
	if len(bounded) != 2 {
		t.Fatalf("bounded literal merge = %d hits, want 2", len(bounded))
	}
}

// TestMergeWithinHitsHonorsRowFilters proves the literal path applies the role
// and time conditions with engine semantics: case-insensitive roles, inclusive
// from, exclusive until.
func TestMergeWithinHitsHonorsRowFilters(t *testing.T) {
	t.Parallel()

	messages := withinTestMessages()
	opts := emptyWithinOptions()
	opts.Roles = []string{"USER"}
	hits := mergeWithinHits(nil, messages, "needle", opts, 10, true)
	if len(hits) != 3 {
		t.Fatalf("role-filtered hits = %d, want 3 user needles", len(hits))
	}

	opts = emptyWithinOptions()
	opts.FromUnix = 1002
	opts.UntilUnix = 1004
	hits = mergeWithinHits(nil, messages, "needle", opts, 10, true)
	if len(hits) != 1 || hits[0].MessageIndex != 2 {
		t.Fatalf("time-bounded hits = %+v, want only #2 (from inclusive, until exclusive)", hits)
	}
}

// TestWindowTextClampsAndMarks proves the hit window clamps at transcript
// edges and marks the hit line.
func TestWindowTextClampsAndMarks(t *testing.T) {
	t.Parallel()

	messages := withinTestMessages()
	head := windowText(messages, 0)
	if strings.Contains(head, "#3") {
		t.Fatalf("window at #0 reached #3:\n%s", head)
	}
	if !strings.Contains(head, ">[#0]") {
		t.Fatalf("window at #0 does not mark the hit line:\n%s", head)
	}
	tail := windowText(messages, 5)
	if !strings.Contains(tail, "[#3]") || strings.Contains(tail, "#6") {
		t.Fatalf("window at #5 bounds wrong:\n%s", tail)
	}
}

// TestBuildWithinResultSetCarriesProvenance proves the stored excerpts carry
// message index, source, and score so analyze and later readers see where each
// match came from.
func TestBuildWithinResultSetCarriesProvenance(t *testing.T) {
	t.Parallel()

	messages := withinTestMessages()
	hits := []withinHit{
		{MessageIndex: 4, Source: hitSourceSemantic, Score: 0.9},
		{MessageIndex: 2, Source: hitSourceLiteral, Score: 0},
	}
	stored := buildWithinResultSet("conv-test", messages, hits)
	if stored.ConversationID != "conv-test" || len(stored.Excerpts) != 2 {
		t.Fatalf("stored = %+v, want 2 excerpts for conv-test", stored)
	}
	first := stored.Excerpts[0]
	if first.MessageIndex != 4 || first.Source != hitSourceSemantic || first.Score != 0.9 {
		t.Fatalf("first excerpt provenance = %+v, want semantic #4 score 0.9", first)
	}
	if !strings.Contains(first.Text, "epsilon needle") {
		t.Fatalf("first excerpt text missing hit message:\n%s", first.Text)
	}
	if !strings.Contains(first.Text, "gamma needle") {
		t.Fatalf("first excerpt text missing window context:\n%s", first.Text)
	}
}
