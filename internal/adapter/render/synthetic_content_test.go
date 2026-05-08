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
	if parts[0].Encrypted != "" {
		t.Fatalf("legacy marker should yield empty encrypted, got %q", parts[0].Encrypted)
	}
}

func TestSyntheticContentCloseWithEncryptedEmbedsAttribute(t *testing.T) {
	close := SyntheticContentCloseWithAttrs(SyntheticReasoning, "ENCBYTES", "")
	if !strings.Contains(close, `<!--/clyde-thinking data-encrypted="ENCBYTES"-->`) {
		t.Fatalf("close with encrypted should embed attribute: %q", close)
	}
}

func TestSyntheticContentCloseWithEmptyEncryptedMatchesLegacyShape(t *testing.T) {
	withEmpty := SyntheticContentCloseWithAttrs(SyntheticReasoning, "", "")
	legacy := SyntheticContentClose(SyntheticReasoning)
	if withEmpty != legacy {
		t.Fatalf("empty encrypted must match legacy close shape:\n with-encrypted: %q\n legacy:        %q", withEmpty, legacy)
	}
}

// End-to-end round-trip: a marker emitted with both data-ref on the open
// AND data-encrypted on the close survives ExtractSyntheticParts unchanged.
// This is the contract Cursor's transcript relies on to carry the
// codex-rs encrypted_content blob between turns without an external store.
func TestExtractSyntheticPartsRoundTripsEncryptedAttribute(t *testing.T) {
	in := SyntheticContentOpenWithRef(SyntheticReasoning, "rs_inline") +
		formatSyntheticBody(syntheticContentSpecs[SyntheticReasoning], "deep thoughts", true) +
		SyntheticContentCloseWithAttrs(SyntheticReasoning, "OPAQUE_BASE64_BLOB==", "")
	parts := ExtractSyntheticParts(in)
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d: %#v", len(parts), parts)
	}
	if parts[0].Kind != SyntheticKindThinking {
		t.Fatalf("kind=%q want %q", parts[0].Kind, SyntheticKindThinking)
	}
	if parts[0].Ref != "rs_inline" {
		t.Fatalf("ref=%q want rs_inline", parts[0].Ref)
	}
	if parts[0].Body != "deep thoughts" {
		t.Fatalf("body=%q want deep thoughts", parts[0].Body)
	}
	if parts[0].Encrypted != "OPAQUE_BASE64_BLOB==" {
		t.Fatalf("encrypted=%q want OPAQUE_BASE64_BLOB==", parts[0].Encrypted)
	}
}

// TestSyntheticContentCloseWithAttrsEmbedsSignatureAttribute asserts that
// a non-empty signature is rendered as the new sibling `data-signature`
// attribute on the close marker (peer of `data-encrypted`). This is the
// Anthropic per-thinking-block carrier the inbound mapper reads on the
// next turn so the round-tripped thinking content block passes signature
// validation upstream.
func TestSyntheticContentCloseWithAttrsEmbedsSignatureAttribute(t *testing.T) {
	close := SyntheticContentCloseWithAttrs(SyntheticReasoning, "", "SIGBYTES")
	if !strings.Contains(close, `<!--/clyde-thinking data-signature="SIGBYTES"-->`) {
		t.Fatalf("close with signature should embed attribute: %q", close)
	}
}

// TestExtractSyntheticPartsRoundTripsSignatureAttribute is the
// signature-only twin of the encrypted-only round trip: a thinking
// envelope with `data-signature` on the close survives through
// ExtractSyntheticParts as the Signature field on the produced part.
func TestExtractSyntheticPartsRoundTripsSignatureAttribute(t *testing.T) {
	in := SyntheticContentOpen(SyntheticReasoning) +
		formatSyntheticBody(syntheticContentSpecs[SyntheticReasoning], "deliberation", true) +
		SyntheticContentCloseWithAttrs(SyntheticReasoning, "", "SIG_VALUE_BASE64==")
	parts := ExtractSyntheticParts(in)
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d: %#v", len(parts), parts)
	}
	if parts[0].Kind != SyntheticKindThinking {
		t.Fatalf("kind=%q want %q", parts[0].Kind, SyntheticKindThinking)
	}
	if parts[0].Body != "deliberation" {
		t.Fatalf("body=%q want deliberation", parts[0].Body)
	}
	if parts[0].Encrypted != "" {
		t.Fatalf("encrypted=%q want empty when only signature is set", parts[0].Encrypted)
	}
	if parts[0].Signature != "SIG_VALUE_BASE64==" {
		t.Fatalf("signature=%q want SIG_VALUE_BASE64==", parts[0].Signature)
	}
}

// TestExtractSyntheticPartsCoexistsEncryptedAndSignature asserts that a
// close marker carrying BOTH `data-encrypted` (Codex) and
// `data-signature` (Anthropic) is parsed without conflict. Both
// providers write the attribute they own and the other is left empty;
// the captureRE preserves the fixed order (encrypted first, signature
// second) that the init() comments document.
func TestExtractSyntheticPartsCoexistsEncryptedAndSignature(t *testing.T) {
	in := SyntheticContentOpenWithRef(SyntheticReasoning, "rs_co") +
		formatSyntheticBody(syntheticContentSpecs[SyntheticReasoning], "co thinking", true) +
		SyntheticContentCloseWithAttrs(SyntheticReasoning, "ENC_VALUE_BASE64==", "SIG_VALUE_BASE64==")
	parts := ExtractSyntheticParts(in)
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d: %#v", len(parts), parts)
	}
	got := parts[0]
	if got.Ref != "rs_co" {
		t.Fatalf("ref=%q want rs_co", got.Ref)
	}
	if got.Body != "co thinking" {
		t.Fatalf("body=%q want co thinking", got.Body)
	}
	if got.Encrypted != "ENC_VALUE_BASE64==" {
		t.Fatalf("encrypted=%q want ENC_VALUE_BASE64==", got.Encrypted)
	}
	if got.Signature != "SIG_VALUE_BASE64==" {
		t.Fatalf("signature=%q want SIG_VALUE_BASE64==", got.Signature)
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
	open := FormatSyntheticContentDeltaWithRef(SyntheticReasoning, true, "", "first")
	if !strings.HasPrefix(open, "<!--clyde-thinking-->") {
		t.Fatalf("first delta missing open marker: %q", open)
	}
	if !strings.HasSuffix(open, "> first") {
		t.Fatalf("open delta should start fresh blockquote line: %q", open)
	}
	cont := FormatSyntheticContentDeltaWithRef(SyntheticReasoning, false, "", "\nsecond")
	if strings.Contains(cont, "<!--clyde-thinking-->") {
		t.Fatalf("continuation delta should not re-emit open marker: %q", cont)
	}
	if cont != "\n> second" {
		t.Fatalf("continuation should turn its own newlines into quoted lines: %q", cont)
	}
}
