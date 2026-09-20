package reorientinject

import (
	"strings"
	"testing"

	"goodkind.io/clyde/internal/tokencount"
)

// testCounter is the counter clyde conversation export uses for Claude.
func testCounter() tokencount.Counter {
	return tokencount.LocalCounter(tokencount.FamilyClaude, "claude-opus-5", tokencount.Settings{})
}

func everyKind(SegmentKind) bool { return true }

// repeatedWords returns text long enough for the counter to measure in the
// hundreds of tokens.
func repeatedWords(word string, count int) string {
	return strings.TrimSpace(strings.Repeat(word+" ", count))
}

// conversation builds a request whose message 1 is far larger than the rest.
// That is the shape a compacted session sends: message 1 stores the prior
// summary and the transcript injected below it.
func conversation() ParsedRequest {
	return ParsedRequest{
		SessionID: "test-session",
		Messages: []Message{
			{Role: "user", Segments: []Segment{{Kind: KindText, Text: "start"}}},
			{Role: "assistant", Segments: []Segment{
				{Kind: KindText, Text: repeatedWords("prior", 4000)},
			}},
			{Role: "user", Segments: []Segment{{Kind: KindText, Text: "run the tests"}}},
			{Role: "assistant", Segments: []Segment{
				{Kind: KindToolUse, Text: `{"command":"go test ./..."}`},
			}},
			{Role: "user", Segments: []Segment{
				{Kind: KindToolResult, Text: repeatedWords("output", 200)},
			}},
			{Role: "assistant", Segments: []Segment{{Kind: KindText, Text: "all green"}}},
			{Role: "user", Segments: []Segment{{Kind: KindText, Text: "compaction prompt"}}},
		},
		InstructionStart: 6,
		Arguments:        nil,
	}
}

func TestPlanCutRetainsStrictlyUnderTheBudget(t *testing.T) {
	t.Parallel()
	counter := testCounter()
	for _, budget := range []int{50, 200, 1000, 5000} {
		got, ok := planCut(conversation(), budget, counter, everyKind)
		if !ok {
			t.Errorf("budget %d planned no cut", budget)
			continue
		}
		if count := counter.Estimate(got.Retained); count >= budget {
			t.Errorf("budget %d retained %d tokens", budget, count)
		}
	}
}

func TestPlanCutRetainsTheTailOfALargeMessage(t *testing.T) {
	t.Parallel()
	got, ok := planCut(conversation(), 1000, testCounter(), everyKind)
	if !ok {
		t.Fatal("planCut planned no cut")
	}
	if got.Cut.MessageIndex != 1 {
		t.Fatalf("cut message index = %d, want 1", got.Cut.MessageIndex)
	}
	if got.Cut.HeadRunes <= 0 {
		t.Fatalf("head runes = %d, want the model to summarize part of message 1", got.Cut.HeadRunes)
	}
	if !strings.Contains(got.Retained, "prior") {
		t.Error("the retained text omits the tail of message 1")
	}
	if !strings.Contains(got.Retained, "all green") {
		t.Error("the retained text omits the newest message")
	}
}

func TestPlanCutNeverRetainsMessageZero(t *testing.T) {
	t.Parallel()
	got, ok := planCut(conversation(), 1_000_000, testCounter(), everyKind)
	if !ok {
		t.Fatal("planCut planned no cut")
	}
	if got.Cut.MessageIndex < 1 {
		t.Fatalf("cut message index = %d, want 1 or greater", got.Cut.MessageIndex)
	}
	if strings.Contains(got.Retained, "start") {
		t.Error("the retained text includes message 0")
	}
}

func TestPlanCutSkipsAnExcludedKind(t *testing.T) {
	t.Parallel()
	chatOnly := func(kind SegmentKind) bool { return kind == KindText }
	got, ok := planCut(conversation(), 1_000_000, testCounter(), chatOnly)
	if !ok {
		t.Fatal("planCut planned no cut")
	}
	if strings.Contains(got.Retained, "go test") {
		t.Error("the retained text includes a tool call the arguments excluded")
	}
	if !strings.Contains(got.Retained, "all green") {
		t.Error("the retained text omits the newest text message")
	}
}

func TestPlanCutReportsNoCutForATinyBudget(t *testing.T) {
	t.Parallel()
	if _, ok := planCut(conversation(), 1, testCounter(), everyKind); ok {
		t.Fatal("a one token budget planned a cut")
	}
}
