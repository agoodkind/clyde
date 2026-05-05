package codex

import (
	"strings"
	"testing"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func TestSanitizeForUpstreamCacheStripsSyntheticThinkingMarker(t *testing.T) {
	in := "<!--clyde-thinking--><details><summary><sub>💭 Thinking…</sub></summary>\n\n<sub>line one\nline two</sub></details><!--/clyde-thinking-->\n\nAnswer: 42"
	got := SanitizeForUpstreamCache(in)
	want := "Answer: 42"
	if got != want {
		t.Fatalf("strip mismatch:\n got  %q\n want %q", got, want)
	}
}

func TestSanitizeForUpstreamCachePassThrough(t *testing.T) {
	cases := []string{
		"",
		"Just a plain answer.",
		"> A blockquote the user wrote, not our marker",
		"<details><summary>User-authored details, no sentinel</summary>preserved</details>",
		"Answer with <details> tags that are NOT ours, <details>nested</details>",
	}
	for _, in := range cases {
		if got := SanitizeForUpstreamCache(in); got != in {
			t.Errorf("SanitizeForUpstreamCache(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestSanitizeForUpstreamCacheHandlesMultipleSyntheticBlocks(t *testing.T) {
	in := "<!--clyde-thinking--><details>first reasoning pass</details><!--/clyde-thinking-->\n\nFirst answer.\n\n<!--clyde-thinking--><details>second reasoning pass</details><!--/clyde-thinking-->\n\nSecond answer."
	got := SanitizeForUpstreamCache(in)
	want := "First answer.\n\nSecond answer."
	if got != want {
		t.Fatalf("multi-block strip mismatch:\n got  %q\n want %q", got, want)
	}
}

func TestSanitizeForUpstreamCachePreservesLegitimateDetails(t *testing.T) {
	in := "Here's the plan. <details><summary>Optional steps</summary>step a\nstep b</details> Done."
	got := SanitizeForUpstreamCache(in)
	if got != in {
		t.Fatalf("should not strip non-sentinel <details>:\n got  %q\n want %q", got, in)
	}
}

// TestSanitizeForUpstreamCacheRoundTripDropsThinkingByDefault asserts the
// Phase B P0 contract for Codex: with default
// inbound_thinking_materialization=drop, marker-wrapped thinking content
// replayed from Cursor is removed before forwarding upstream and the
// surrounding prose survives in order. This is the typed-parts-aware
// equivalent of the older "thinking marker is stripped" check; its
// existence keeps the Codex path on the [adapterrender.ExtractSyntheticParts]
// API so a future flip to plain_text_concat is a one-line change.
func TestSanitizeForUpstreamCacheRoundTripDropsThinkingByDefault(t *testing.T) {
	thinking := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "private chain of thought")
	in := "Pre.\n\n" + thinking + "Post."
	got := SanitizeForUpstreamCache(in)
	if strings.Contains(got, "clyde-thinking") || strings.Contains(got, "private chain of thought") {
		t.Fatalf("thinking content leaked upstream: %q", got)
	}
	if !strings.Contains(got, "Pre.") || !strings.Contains(got, "Post.") {
		t.Fatalf("surrounding prose lost: %q", got)
	}
}

// TestSanitizeForUpstreamCacheWithStrategyPlainTextConcatPreservesThinking
// verifies that operators who flip Codex's inbound_thinking_materialization
// from the default `drop` to `plain_text_concat` actually get the thinking
// content preserved as plain prose for the model. The lever is documented
// at config.AdapterSyntheticContent.Codex.InboundThinkingMaterialization.
func TestSanitizeForUpstreamCacheWithStrategyPlainTextConcatPreservesThinking(t *testing.T) {
	thinking := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "the model considered three options")
	in := "Pre.\n\n" + thinking + "Post."
	got := SanitizeForUpstreamCacheWithStrategy(in, adapterrender.MaterializePlainTextConcat)
	if strings.Contains(got, "clyde-thinking") {
		t.Fatalf("envelope marker leaked upstream: %q", got)
	}
	if !strings.Contains(got, "the model considered three options") {
		t.Fatalf("thinking content was dropped under plain_text_concat: %q", got)
	}
	if !strings.Contains(got, "Pre.") || !strings.Contains(got, "Post.") {
		t.Fatalf("surrounding prose lost: %q", got)
	}
}

// TestSanitizeForUpstreamCacheWithStrategyDropMatchesDefault asserts that the
// explicit drop strategy reproduces today's default-drop behavior so a
// rollback (operator flips back to drop) is a no-op contract change.
func TestSanitizeForUpstreamCacheWithStrategyDropMatchesDefault(t *testing.T) {
	thinking := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "scratchpad")
	in := "Before.\n\n" + thinking + "After."
	def := SanitizeForUpstreamCache(in)
	explicit := SanitizeForUpstreamCacheWithStrategy(in, adapterrender.MaterializeDrop)
	if def != explicit {
		t.Fatalf("default and explicit drop disagree:\n default  %q\n explicit %q", def, explicit)
	}
}

func TestSanitizeForUpstreamCacheStripsBothReasoningAndNoticeEnvelopes(t *testing.T) {
	thinking := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "internal scratch")
	notice := adapterrender.FormatSyntheticContent(adapterrender.SyntheticNotice, "⚠️ quota notice")
	in := thinking + "\n\nReal answer.\n\n" + notice + "trailing answer."
	got := SanitizeForUpstreamCache(in)
	if strings.Contains(got, "clyde-thinking") || strings.Contains(got, "clyde-notice") {
		t.Fatalf("synthetic markers leaked: %q", got)
	}
	if !strings.Contains(got, "Real answer.") || !strings.Contains(got, "trailing answer.") {
		t.Fatalf("answer text lost: %q", got)
	}
}
