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

// compactedSession builds a request whose message 1 is far larger than the
// rest. That is the shape a compacted session sends: message 1 stores the prior
// summary and the transcript injected below it.
func compactedSession() ParsedRequest {
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
		got, ok := planCut(compactedSession(), budget, 0, counter, everyKind)
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
	got, ok := planCut(compactedSession(), 1000, 0, testCounter(), everyKind)
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

// TestPlanCutRetainsTheTailOfMessageZero pins the common case: after one
// compaction, message 0 stores the prior summary and the prior injection and is
// the largest message in the request. A budget larger than every later message
// must cut inside message 0 and retain its tail.
func TestPlanCutRetainsTheTailOfMessageZero(t *testing.T) {
	t.Parallel()
	request := compactedSession()
	request.Messages[0] = Message{Role: RoleUser, Segments: []Segment{
		{Kind: KindText, Text: repeatedWords("summary", 3000) + " prior-injection-tail"},
	}}
	// Messages 1 through 5 measure about 5,500 tokens and message 0 about
	// 3,900, so an 8,000 budget retains every later message and then part of
	// message 0.
	got, ok := planCut(request, 8000, 0, testCounter(), everyKind)
	if !ok {
		t.Fatal("planCut planned no cut")
	}
	if got.Cut.MessageIndex != 0 {
		t.Fatalf("cut message index = %d, want 0", got.Cut.MessageIndex)
	}
	if got.Cut.HeadRunes <= 0 {
		t.Fatalf("head runes = %d, want the model to summarize part of message 0", got.Cut.HeadRunes)
	}
	if !strings.Contains(got.Retained, "prior-injection-tail") {
		t.Error("the retained text omits the tail of message 0")
	}
	if !strings.Contains(got.Retained, "all green") {
		t.Error("the retained text omits the newest message")
	}
}

// TestPlanCutSendsMessageZeroWholeWhenEverythingFits pins the other case: the
// model always needs content to summarize, so a budget larger than the whole
// conversation keeps message 0 in the forwarded request and retains the rest.
func TestPlanCutSendsMessageZeroWholeWhenEverythingFits(t *testing.T) {
	t.Parallel()
	got, ok := planCut(compactedSession(), 1_000_000, 0, testCounter(), everyKind)
	if !ok {
		t.Fatal("planCut planned no cut")
	}
	if got.Cut.MessageIndex != 1 || got.Cut.HeadRunes != 0 {
		t.Fatalf("cut = %+v, want message 1 segment 0 head 0", got.Cut)
	}
	if strings.Contains(got.Retained, "start") {
		t.Error("the retained text includes message 0")
	}
}

func TestPlanCutSkipsAnExcludedKind(t *testing.T) {
	t.Parallel()
	chatOnly := func(kind SegmentKind) bool { return kind == KindText }
	got, ok := planCut(compactedSession(), 1_000_000, 0, testCounter(), chatOnly)
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
	if _, ok := planCut(compactedSession(), 1, 0, testCounter(), everyKind); ok {
		t.Fatal("a one token budget planned a cut")
	}
}
