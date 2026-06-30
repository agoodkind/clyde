package codex

import (
	"log/slog"
	"strings"
	"testing"
)

func TestDetectRepeatedAssistantTextExactBlock(t *testing.T) {
	got := detectRepeatedAssistantText("alpha betaalpha beta")
	if !got.Detected {
		t.Fatalf("Detected=false want true")
	}
	if got.BlockCount != 2 {
		t.Fatalf("BlockCount=%d want 2", got.BlockCount)
	}
	if got.BlockChars != len([]rune("alpha beta")) {
		t.Fatalf("BlockChars=%d want %d", got.BlockChars, len([]rune("alpha beta")))
	}
	if got.BlockSHA256 != sha256StringHex("alpha beta") {
		t.Fatalf("BlockSHA256=%q want %q", got.BlockSHA256, sha256StringHex("alpha beta"))
	}
	if got.BlockPreview != "alpha beta" {
		t.Fatalf("BlockPreview=%q want alpha beta", got.BlockPreview)
	}
}

func TestDetectRepeatedAssistantTextWhitespaceSeparatedBlock(t *testing.T) {
	got := detectRepeatedAssistantText("alpha beta\n\nalpha   beta")
	if !got.Detected {
		t.Fatalf("Detected=false want true")
	}
	if got.BlockCount != 2 || got.BlockChars != len([]rune("alpha beta")) {
		t.Fatalf("block count/chars=%d/%d want 2/%d", got.BlockCount, got.BlockChars, len([]rune("alpha beta")))
	}
}

func TestDetectRepeatedAssistantTextPrefixSuffixOnly(t *testing.T) {
	got := detectRepeatedAssistantText("prefix middle prefix")
	if got.Detected {
		t.Fatalf("Detected=true want false")
	}
	if got.PrefixSuffixChars != len([]rune("prefix")) {
		t.Fatalf("PrefixSuffixChars=%d want %d", got.PrefixSuffixChars, len([]rune("prefix")))
	}
}

func TestAssistantTextDeltaAggregateFingerprintUsesJoinedDeltas(t *testing.T) {
	var agg assistantTextDeltaAggregate
	agg.Add("The final answer")
	agg.Add(" is duplicated.")
	agg.Add("\nThe final answer is duplicated.")

	attrs := agg.toSlogAttrs()
	wantNormalized := "The final answer is duplicated. The final answer is duplicated."
	if got := stringAttr(attrs, "assistant_text_normalized_sha256"); got != sha256StringHex(wantNormalized) {
		t.Fatalf("assistant_text_normalized_sha256=%q want %q", got, sha256StringHex(wantNormalized))
	}
	if got := intAttr(attrs, "assistant_text_delta_count"); got != 3 {
		t.Fatalf("assistant_text_delta_count=%d want 3", got)
	}
	if got := intAttr(attrs, "assistant_text_delta_chars"); got != len([]rune("The final answer is duplicated.\nThe final answer is duplicated.")) {
		t.Fatalf("assistant_text_delta_chars=%d", got)
	}
	if !boolAttr(attrs, "assistant_text_repeated_block_detected") {
		t.Fatalf("assistant_text_repeated_block_detected=false want true")
	}
	if got := stringAttr(attrs, "assistant_text_first_preview"); got != "The final answer" {
		t.Fatalf("assistant_text_first_preview=%q want first delta preview", got)
	}
	if got := stringAttr(attrs, "assistant_text_last_preview"); got != "The final answer is duplicated." {
		t.Fatalf("assistant_text_last_preview=%q want last delta preview", got)
	}
}

func TestAssistantTextDeltaAggregatePreviewIsCapped(t *testing.T) {
	var agg assistantTextDeltaAggregate
	agg.Add(strings.Repeat("x", assistantDeltaPreviewRunes+25))

	attrs := agg.toSlogAttrs()
	if got := len([]rune(stringAttr(attrs, "assistant_text_first_preview"))); got != assistantDeltaPreviewRunes {
		t.Fatalf("first preview runes=%d want %d", got, assistantDeltaPreviewRunes)
	}
	if got := len([]rune(stringAttr(attrs, "assistant_text_last_preview"))); got != assistantDeltaPreviewRunes {
		t.Fatalf("last preview runes=%d want %d", got, assistantDeltaPreviewRunes)
	}
}

func stringAttr(attrs []slog.Attr, key string) string {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.String()
		}
	}
	return ""
}

func intAttr(attrs []slog.Attr, key string) int {
	for _, attr := range attrs {
		if attr.Key == key {
			return int(attr.Value.Int64())
		}
	}
	return 0
}

func boolAttr(attrs []slog.Attr, key string) bool {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.Bool()
		}
	}
	return false
}
