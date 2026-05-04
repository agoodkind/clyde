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
