package render

import (
	"strings"
	"testing"
)

func TestFormatSyntheticContentReasoningWrapsBody(t *testing.T) {
	got := FormatSyntheticContent(SyntheticReasoning, "first line\nsecond line")
	if !strings.Contains(got, "<!--clyde-thinking-->") {
		t.Fatalf("missing reasoning open marker: %q", got)
	}
	if !strings.Contains(got, "<!--/clyde-thinking-->") {
		t.Fatalf("missing reasoning close marker: %q", got)
	}
	if !strings.Contains(got, "> first line\n> second line") {
		t.Fatalf("missing blockquote body: %q", got)
	}
}

func TestFormatSyntheticContentNoticeWrapsBody(t *testing.T) {
	got := FormatSyntheticContent(SyntheticNotice, "⚠️ You have about 7% left.")
	if !strings.Contains(got, "<!--clyde-notice-->") || !strings.Contains(got, "<!--/clyde-notice-->") {
		t.Fatalf("missing notice markers: %q", got)
	}
	if !strings.Contains(got, "> ⚠️ You have about 7% left.") {
		t.Fatalf("missing blockquote body: %q", got)
	}
}

func TestFormatSyntheticContentEmptyBodyReturnsEmpty(t *testing.T) {
	if got := FormatSyntheticContent(SyntheticNotice, "   "); got != "" {
		t.Fatalf("empty body=%q want empty", got)
	}
}

func TestStripSyntheticContentRemovesAllKindsButKeepsAnswer(t *testing.T) {
	thinking := FormatSyntheticContent(SyntheticReasoning, "internal scratch")
	notice := FormatSyntheticContent(SyntheticNotice, "⚠️ Notice text.")
	mixed := thinking + "\n\nFinal answer.\n\n" + notice + "more answer."

	got := StripSyntheticContent(mixed)
	if strings.Contains(got, "clyde-thinking") || strings.Contains(got, "clyde-notice") {
		t.Fatalf("strip left a marker: %q", got)
	}
	if !strings.Contains(got, "Final answer.") || !strings.Contains(got, "more answer.") {
		t.Fatalf("strip removed real text: %q", got)
	}
}

func TestStripSyntheticContentIsIdempotentAndSafeWithoutMarkers(t *testing.T) {
	answer := "Just a regular answer with no markers."
	if got := StripSyntheticContent(answer); got != answer {
		t.Fatalf("strip mutated text: %q", got)
	}
	if got := StripSyntheticContent(""); got != "" {
		t.Fatalf("empty strip mutated: %q", got)
	}
}

func TestExtractSyntheticPartsSingleThinking(t *testing.T) {
	in := FormatSyntheticContent(SyntheticReasoning, "internal scratch")
	parts := ExtractSyntheticParts(in)
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d: %#v", len(parts), parts)
	}
	if parts[0].Kind != SyntheticKindThinking {
		t.Fatalf("kind=%q want %q", parts[0].Kind, SyntheticKindThinking)
	}
	if parts[0].Body != "internal scratch" {
		t.Fatalf("body=%q want %q", parts[0].Body, "internal scratch")
	}
}

func TestExtractSyntheticPartsMixedTextThinkingText(t *testing.T) {
	thinking := FormatSyntheticContent(SyntheticReasoning, "the model thought hard")
	in := "Hello there.\n\n" + thinking + "\n\nFinal answer."
	parts := ExtractSyntheticParts(in)
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d: %#v", len(parts), parts)
	}
	if parts[0].Kind != SyntheticKindText || !strings.HasPrefix(parts[0].Body, "Hello there.") {
		t.Fatalf("part0=%#v", parts[0])
	}
	if parts[1].Kind != SyntheticKindThinking || parts[1].Body != "the model thought hard" {
		t.Fatalf("part1=%#v", parts[1])
	}
	if parts[2].Kind != SyntheticKindText || !strings.Contains(parts[2].Body, "Final answer.") {
		t.Fatalf("part2=%#v", parts[2])
	}
}

func TestExtractSyntheticPartsMultipleConsecutiveThinking(t *testing.T) {
	a := FormatSyntheticContent(SyntheticReasoning, "first pass")
	b := FormatSyntheticContent(SyntheticReasoning, "second pass")
	in := a + b + "Done."
	parts := ExtractSyntheticParts(in)
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d: %#v", len(parts), parts)
	}
	if parts[0].Kind != SyntheticKindThinking || parts[0].Body != "first pass" {
		t.Fatalf("part0=%#v", parts[0])
	}
	if parts[1].Kind != SyntheticKindThinking || parts[1].Body != "second pass" {
		t.Fatalf("part1=%#v", parts[1])
	}
	if parts[2].Kind != SyntheticKindText || parts[2].Body != "Done." {
		t.Fatalf("part2=%#v", parts[2])
	}
}

func TestExtractSyntheticPartsNoticeMaterializes(t *testing.T) {
	notice := FormatSyntheticContent(SyntheticNotice, "⚠️ heads up")
	parts := ExtractSyntheticParts(notice + "Body.")
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d: %#v", len(parts), parts)
	}
	if parts[0].Kind != SyntheticKindNotice || parts[0].Body != "⚠️ heads up" {
		t.Fatalf("part0=%#v", parts[0])
	}
	if parts[1].Kind != SyntheticKindText || parts[1].Body != "Body." {
		t.Fatalf("part1=%#v", parts[1])
	}
}

func TestExtractSyntheticPartsIdempotentOnPlainText(t *testing.T) {
	in := "Just a normal answer."
	parts := ExtractSyntheticParts(in)
	if len(parts) != 1 || parts[0].Kind != SyntheticKindText || parts[0].Body != in {
		t.Fatalf("plain text round trip failed: %#v", parts)
	}
}

func TestExtractSyntheticPartsEmptyReturnsNil(t *testing.T) {
	if got := ExtractSyntheticParts(""); got != nil {
		t.Fatalf("empty input should return nil, got %#v", got)
	}
}

func TestStripSyntheticContentMatchesExtractTextJoin(t *testing.T) {
	thinking := FormatSyntheticContent(SyntheticReasoning, "scratch")
	notice := FormatSyntheticContent(SyntheticNotice, "notice body")
	in := thinking + "\n\nFinal answer.\n\n" + notice + "trailing."

	stripped := StripSyntheticContent(in)
	parts := ExtractSyntheticParts(in)
	var joined strings.Builder
	for _, p := range parts {
		if p.Kind == SyntheticKindText {
			joined.WriteString(p.Body)
		}
	}
	if joined.String() != stripped {
		t.Fatalf("strip and extract-text-join disagree:\n strip=%q\n join=%q", stripped, joined.String())
	}
}

func TestFormatSyntheticContentDeltaContinuesOpenBlockWithoutHeader(t *testing.T) {
	open := FormatSyntheticContentDelta(SyntheticReasoning, true, "first")
	if !strings.HasPrefix(open, "<!--clyde-thinking-->") {
		t.Fatalf("first delta missing open marker: %q", open)
	}
	if !strings.HasSuffix(open, "> first") {
		t.Fatalf("open delta should start fresh blockquote line: %q", open)
	}
	cont := FormatSyntheticContentDelta(SyntheticReasoning, false, "\nsecond")
	if strings.Contains(cont, "<!--clyde-thinking-->") {
		t.Fatalf("continuation delta should not re-emit open marker: %q", cont)
	}
	if cont != "\n> second" {
		t.Fatalf("continuation should turn its own newlines into quoted lines: %q", cont)
	}
}
