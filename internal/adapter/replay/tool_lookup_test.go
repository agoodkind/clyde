package replay_test

import (
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/replay"
)

func TestToolFingerprintRequiresEveryCallID(t *testing.T) {
	t.Parallel()
	first := openai.ToolCall{
		ID:       "one",
		Function: openai.ToolCallFunction{Name: "read", Arguments: "first"},
	}
	second := openai.ToolCall{
		ID:       "two",
		Function: openai.ToolCallFunction{Name: "read", Arguments: "second"},
	}
	digest := replay.ToolFingerprint([]openai.ToolCall{first, second})
	if digest == "" || digest == replay.ToolFingerprint([]openai.ToolCall{second, first}) {
		t.Fatal("the tool digest did not identify the ordered calls")
	}

	second.Function.Arguments = "changed"
	if digest == replay.ToolFingerprint([]openai.ToolCall{first, second}) {
		t.Fatal("the tool digest did not change with the arguments")
	}

	second.ID = ""
	if replay.ToolFingerprint(nil) != "" ||
		replay.ToolFingerprint([]openai.ToolCall{first, second}) != "" {
		t.Fatal("computed a tool fingerprint without every call ID")
	}
}

func TestFindToolReplyReadsEarlierSchemaAndRequiresUniqueMatch(t *testing.T) {
	t.Parallel()
	store := replay.Store{Path: filepath.Join(t.TempDir(), "replay.db")}
	first := replay.Record{
		Reference:     replay.Reference{ConversationID: "first chat", ReplyID: "first reply"},
		Provider:      "codex",
		Digest:        "reply digest",
		VisibleDigest: "visible digest",
	}
	ctx := t.Context()
	if err := store.Save(ctx, first); err != nil {
		t.Fatal(err)
	}

	const digest = "tool digest"
	checkLookup := func(conversation string, want replay.Reference) {
		t.Helper()
		record, found, err := store.FindToolReply(ctx, conversation, digest)
		if err != nil || found != (want.ReplyID != "") || record.Reference != want {
			t.Fatalf(
				"lookup in %q returned %+v, found=%t, err=%v",
				conversation,
				record.Reference,
				found,
				err,
			)
		}

		if found && record.ToolDigest != digest {
			t.Fatal("lookup did not return the saved tool digest")
		}
	}

	var missing replay.Reference
	checkLookup("", missing)
	first.ToolDigest = digest
	if err := store.Save(ctx, first); err != nil {
		t.Fatal(err)
	}

	checkLookup("", first.Reference)
	checkLookup("unknown chat", missing)
	loaded, found, err := store.Load(ctx, first.Reference)
	if err != nil || !found || loaded.ToolDigest != digest {
		t.Fatalf(
			"lookup by reference did not read the saved tool digest, found=%t, err=%v",
			found,
			err,
		)
	}

	conflicting := first
	conflicting.ToolDigest = "different calls"
	if err := store.Save(ctx, conflicting); err == nil {
		t.Fatal("overwrote the indexed tool calls for an existing reply")
	}

	checkLookup("", first.Reference)
	second := first
	second.Reference.ConversationID = "second chat"
	if err := store.Save(ctx, second); err != nil {
		t.Fatal(err)
	}

	checkLookup("", missing)
	checkLookup("first chat", first.Reference)
	checkLookup("second chat", second.Reference)
	first.Reference.ReplyID = "retry"
	if err := store.Save(ctx, first); err != nil {
		t.Fatal(err)
	}

	checkLookup("first chat", missing)
}
