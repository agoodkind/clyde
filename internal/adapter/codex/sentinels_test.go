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
