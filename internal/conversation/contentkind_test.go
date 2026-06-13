package conversation

import (
	"slices"
	"testing"
)

func TestExpandContentSelector(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value string
		want  []ContentKind
		ok    bool
	}{
		{"chat", []ContentKind{ContentKindChat}, true},
		{"raw_json_metadata", []ContentKind{ContentKindRawJSONMetadata}, true},
		{"tools", []ContentKind{ContentKindToolCalls, ContentKindToolOutputs}, true},
		{"all", AllContentKinds, true},
		{"bogus", nil, false},
		{"", nil, false},
	}
	for _, tc := range cases {
		got, ok := ExpandContentSelector(tc.value)
		if ok != tc.ok {
			t.Errorf("%q: ok = %t, want %t", tc.value, ok, tc.ok)
		}
		if ok && !slices.Equal(got, tc.want) {
			t.Errorf("%q: got %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestResolveContentKinds(t *testing.T) {
	t.Parallel()
	set, err := ResolveContentKinds([]string{"chat", "tools", "thinking"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []ContentKind{
		ContentKindChat, ContentKindThinking, ContentKindToolCalls, ContentKindToolOutputs,
	}
	if got := set.Kinds(); !slices.Equal(got, want) {
		t.Errorf("kinds = %v, want %v", got, want)
	}

	allSet, err := ResolveContentKinds([]string{"all"})
	if err != nil {
		t.Fatalf("resolve all: %v", err)
	}
	if got := allSet.Kinds(); !slices.Equal(got, AllContentKinds) {
		t.Errorf("all kinds = %v, want %v", got, AllContentKinds)
	}

	if _, err := ResolveContentKinds([]string{"bogus"}); err == nil {
		t.Error("expected error for unknown kind")
	}
	if _, err := ResolveContentKinds(nil); err == nil {
		t.Error("expected error for empty selection")
	}
	if _, err := ResolveContentKinds([]string{"", "  "}); err == nil {
		t.Error("expected error for blank-only selection")
	}
}

func TestContentKindSetHasAndKindsOrder(t *testing.T) {
	t.Parallel()
	set := NewContentKindSet(ContentKindRawJSONMetadata, ContentKindChat)
	if !set.Has(ContentKindChat) || !set.Has(ContentKindRawJSONMetadata) {
		t.Error("set missing expected members")
	}
	if set.Has(ContentKindThinking) {
		t.Error("set has unexpected member")
	}
	// Kinds returns canonical order regardless of insertion order.
	if got := set.Kinds(); !slices.Equal(got, []ContentKind{ContentKindChat, ContentKindRawJSONMetadata}) {
		t.Errorf("kinds order = %v", got)
	}
	if NewContentKindSet().Empty() != true {
		t.Error("empty set should report Empty")
	}
	var zero ContentKindSet
	if zero.Has(ContentKindChat) {
		t.Error("zero set should have no members")
	}
}
