package conversation

import (
	"strings"
	"testing"
)

// TestLoadRulesTagRoundTrip pins the tag contract: rendering a kind set and
// parsing the tag back yields the load gate that kind set implies, for the
// default set, each load-gated kind alone, and all four together.
func TestLoadRulesTagRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		kinds ContentKindSet
		want  LoadOptions
	}{
		{
			name:  "default",
			kinds: NewContentKindSet(ContentKindChat, ContentKindToolCalls),
			want:  LoadOptions{IncludeSystemPrompts: false, IncludeSystemMessages: false, IncludeToolOutputs: false, IncludeInjected: false, HarnessTally: nil},
		},
		{
			name:  "system messages",
			kinds: NewContentKindSet(ContentKindChat, ContentKindSystemMessages),
			want:  LoadOptions{IncludeSystemPrompts: false, IncludeSystemMessages: true, IncludeToolOutputs: false, IncludeInjected: false, HarnessTally: nil},
		},
		{
			name:  "injected",
			kinds: NewContentKindSet(ContentKindChat, ContentKindInjected),
			want:  LoadOptions{IncludeSystemPrompts: false, IncludeSystemMessages: false, IncludeToolOutputs: false, IncludeInjected: true, HarnessTally: nil},
		},
		{
			name:  "all gated kinds",
			kinds: NewContentKindSet(ContentKindChat, ContentKindToolOutputs, ContentKindSystemPrompts, ContentKindSystemMessages, ContentKindInjected),
			want:  LoadOptions{IncludeSystemPrompts: true, IncludeSystemMessages: true, IncludeToolOutputs: true, IncludeInjected: true, HarnessTally: nil},
		},
	}
	for _, testCase := range cases {
		tag := LoadRulesTag(testCase.kinds)
		if !strings.HasPrefix(tag, "v1;") {
			t.Fatalf("%s: tag %q lacks the v1 version prefix", testCase.name, tag)
		}
		options, known := LoadOptionsForRules(tag)
		if !known {
			t.Fatalf("%s: tag %q reported unknown", testCase.name, tag)
		}
		if options != testCase.want {
			t.Fatalf("%s: options = %+v, want %+v", testCase.name, options, testCase.want)
		}
	}
}

// TestLoadOptionsForRulesLegacyAndUnknown pins the two fallback contracts: the
// empty tag is a legacy row and maps to the default rules as a known read,
// while an unrecognized version maps to the default rules and reports unknown
// so the caller can log the degraded read.
func TestLoadOptionsForRulesLegacyAndUnknown(t *testing.T) {
	t.Parallel()

	defaultOptions := LoadOptions{IncludeSystemPrompts: false, IncludeSystemMessages: false, IncludeToolOutputs: false, IncludeInjected: false, HarnessTally: nil}

	options, known := LoadOptionsForRules("")
	if !known || options != defaultOptions {
		t.Fatalf("empty tag: options=%+v known=%v, want default and known", options, known)
	}

	options, known = LoadOptionsForRules("v9;future_kind")
	if known {
		t.Fatal("future version tag reported known, want unknown")
	}
	if options != defaultOptions {
		t.Fatalf("future version tag options = %+v, want default", options)
	}
}

// TestContextWindowHonorsHitLoadRules replays the CLYDE-638 skew. The fixture
// yields a system record at index 0 only when system messages are loaded, so a
// row written under indexed_content including system_messages numbers the user
// message as index 1. Passing the hit's tag makes the window land on that
// message; reading the same index under the default rules runs past the end,
// which is the skew the tag exists to remove.
func TestContextWindowHonorsHitLoadRules(t *testing.T) {
	t.Parallel()
	idx, record := newOptionAwareIndex()

	tagged, err := idx.ContextWindowText(record, "", 1, 0, 0, "v1;system_messages")
	if err != nil {
		t.Fatalf("tagged read returned error: %v", err)
	}
	if !strings.Contains(tagged, "visible transcript message") {
		t.Fatalf("tagged read = %q, want the user message index 1 refers to", tagged)
	}

	untagged, err := idx.ContextWindowText(record, "", 1, 0, 0, "")
	if err != nil {
		t.Fatalf("untagged read returned error: %v", err)
	}
	if strings.Contains(untagged, "visible transcript message") {
		t.Fatalf("untagged read = %q: index 1 resolved under the default rules, so the fixture no longer reproduces the skew", untagged)
	}
}
