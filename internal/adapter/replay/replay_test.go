package replay_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/replay"
)

func TestReferenceRoundTrip(t *testing.T) {
	t.Parallel()
	ref := replay.Reference{ConversationID: "héllo-->\nid", ReplyID: replay.NewID()}
	marker := ref.Marker()
	body := "example" + marker + "\n\n"
	visible, got, found := replay.StripReference(body + marker)
	if !found || visible != body || got != ref {
		t.Fatalf("stripping the final marker changed its reference or preceding text")
	}

	visible, got, found = replay.StripReference(body)
	if !found || visible != "example" || got != ref {
		t.Fatal("failed to read the reply reference before trailing whitespace")
	}

	for _, text := range []string{
		body + "literal",
		marker + " literal",
		"text\n\n<!-- clyde-replay:v1:A=.QQ -->",
	} {
		visible, _, found := replay.StripReference(text)
		if found || visible != text {
			t.Errorf("stripped a noncanonical or embedded reference from %q", text)
		}
	}
}

func TestFingerprintIncludesOrderedToolArguments(t *testing.T) {
	t.Parallel()
	call := openai.ToolCall{
		ID:       "first",
		Type:     "function",
		Function: openai.ToolCallFunction{Name: "read", Arguments: "a"},
	}
	calls := []openai.ToolCall{call}
	initial := replay.Fingerprint("result", calls)
	calls[0].Index++
	if replay.Fingerprint("result", calls) != initial {
		t.Fatal("the SSE index changed the reply fingerprint")
	}

	calls[0].Function.Arguments = "b"
	if replay.Fingerprint("result", calls) == initial {
		t.Fatal("different tool arguments produced the same fingerprint")
	}

	other := calls[0]
	if replay.Fingerprint("result", []openai.ToolCall{call, other}) ==
		replay.Fingerprint("result", []openai.ToolCall{other, call}) {
		t.Fatal("reordering calls did not change the fingerprint")
	}
}

func TestStoreConcurrentSaveAndRestart(t *testing.T) {
	t.Parallel()
	store := replay.Store{Path: filepath.Join(t.TempDir(), "state", "replay ?.db")}
	const body = "prefix💭suffix"
	start := len("prefix")
	end := len("prefix💭")
	redacted := render.SyntheticPart{Kind: render.SyntheticRedactedThinking, Encrypted: "opaque"}
	thinking := render.SyntheticPart{
		Kind:      render.SyntheticReasoning,
		Body:      "thought",
		Signature: "signed",
	}
	record := replay.Record{
		Reference:     replay.Reference{ConversationID: "chat", ReplyID: replay.NewID()},
		Provider:      "anthropic",
		Digest:        replay.Fingerprint(body, nil),
		VisibleDigest: replay.TextFingerprint(body),
		Blocks: []replay.Block{
			{Start: start, End: start, Part: redacted},
			{Start: start, End: end, Part: thinking},
		},
	}
	ctx := t.Context()
	if _, found, err := store.Load(ctx, record.Reference); err != nil || found {
		t.Fatalf("lookup in a new store returned found=%v, err=%v", found, err)
	}

	if _, err := os.Stat(filepath.Dir(store.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lookup created the state directory or returned an unexpected error: %v", err)
	}

	var saves sync.WaitGroup

	for range 8 {
		saves.Go(func() {
			if err := store.Save(ctx, record); err != nil {
				t.Errorf("concurrent save failed: %v", err)
			}
		})
	}

	saves.Wait()
	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions are %o, want 600", info.Mode().Perm())
	}

	changed := record
	changed.Provider = "codex"
	if err := store.Save(ctx, changed); err == nil {
		t.Fatal("overwrote an existing reply with different metadata")
	}

	reopened := replay.Store{Path: store.Path}
	got, found, err := reopened.Load(ctx, record.Reference)
	if err != nil || !found || !reflect.DeepEqual(got, record) {
		t.Fatalf("lookup after reopening returned found=%v, err=%v, record=%#v", found, err, got)
	}

	parts, ok := got.Parts(body)
	want := []render.SyntheticPart{
		{Kind: render.SyntheticKindText, Body: "prefix"},
		redacted,
		thinking,
		{Kind: render.SyntheticKindText, Body: "suffix"},
	}
	if !ok || !reflect.DeepEqual(parts, want) {
		t.Fatalf("restored parts = %#v, want %#v", parts, want)
	}

	if _, ok := got.Parts(body + "!"); ok {
		t.Fatal("restored a reply after its visible text changed")
	}

	got.Blocks[0].End = end
	if _, ok := got.Parts(body); ok {
		t.Fatal("restored blocks with overlapping ranges")
	}
}
