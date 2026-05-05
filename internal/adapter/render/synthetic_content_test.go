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

func TestSyntheticContentOpenWithRefEmbedsAttribute(t *testing.T) {
	open := SyntheticContentOpenWithRef(SyntheticReasoning, "rs_abc123")
	if !strings.HasPrefix(open, `<!--clyde-thinking data-ref="rs_abc123"-->`) {
		t.Fatalf("open with ref should prefix marker with data-ref: %q", open)
	}
}

func TestSyntheticContentOpenWithEmptyRefMatchesLegacyShape(t *testing.T) {
	withEmpty := SyntheticContentOpenWithRef(SyntheticReasoning, "")
	legacy := SyntheticContentOpen(SyntheticReasoning)
	if withEmpty != legacy {
		t.Fatalf("empty ref must round-trip to legacy shape:\n with-ref: %q\n legacy:   %q", withEmpty, legacy)
	}
}

func TestExtractSyntheticPartsCarriesRefAttribute(t *testing.T) {
	in := SyntheticContentOpenWithRef(SyntheticReasoning, "rs_xyz789") +
		formatSyntheticBody(syntheticContentSpecs[SyntheticReasoning], "with ref", true) +
		SyntheticContentClose(SyntheticReasoning)
	parts := ExtractSyntheticParts(in)
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d: %#v", len(parts), parts)
	}
	if parts[0].Kind != SyntheticKindThinking {
		t.Fatalf("kind=%q want %q", parts[0].Kind, SyntheticKindThinking)
	}
	if parts[0].Body != "with ref" {
		t.Fatalf("body=%q want %q", parts[0].Body, "with ref")
	}
	if parts[0].Ref != "rs_xyz789" {
		t.Fatalf("ref=%q want %q", parts[0].Ref, "rs_xyz789")
	}
}

func TestExtractSyntheticPartsLegacyMarkerHasEmptyRef(t *testing.T) {
	in := FormatSyntheticContent(SyntheticReasoning, "no ref attribute")
	parts := ExtractSyntheticParts(in)
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d: %#v", len(parts), parts)
	}
	if parts[0].Ref != "" {
		t.Fatalf("legacy marker should yield empty ref, got %q", parts[0].Ref)
	}
}

func TestFormatSyntheticContentDeltaWithRefEmbedsAttributeOnOpen(t *testing.T) {
	open := FormatSyntheticContentDeltaWithRef(SyntheticReasoning, true, "rs_delta", "first")
	if !strings.HasPrefix(open, `<!--clyde-thinking data-ref="rs_delta"-->`) {
		t.Fatalf("open delta with ref must carry attribute: %q", open)
	}
	cont := FormatSyntheticContentDeltaWithRef(SyntheticReasoning, false, "rs_delta", "\nsecond")
	if strings.Contains(cont, "data-ref=") {
		t.Fatalf("continuation delta must not repeat the data-ref attribute: %q", cont)
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
