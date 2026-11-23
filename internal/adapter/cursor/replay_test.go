package cursor

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/replay"
)

func TestPrepareReplayChecksReplyContent(t *testing.T) {
	t.Parallel()

	store := replay.Store{Path: filepath.Join(t.TempDir(), "reply.db")}
	ref := replay.Reference{ConversationID: replay.NewID(), ReplyID: replay.NewID()}
	const visible = "> Thinking\n\nAnswer"
	part := render.SyntheticPart{
		Kind:      render.SyntheticKindThinking,
		Origin:    render.OriginAnthropic,
		Body:      " exact thinking ",
		Signature: "opaque-signature",
	}
	calls := []openai.ToolCall{
		{
			ID:       "call_test",
			Function: openai.ToolCallFunction{Name: "Read", Arguments: `{"path":"test"}`},
		},
	}
	if err := store.Save(
		t.Context(),
		replay.Record{
			Reference:     ref,
			Provider:      "anthropic",
			Digest:        replay.Fingerprint(visible, calls),
			VisibleDigest: replay.TextFingerprint(visible),
			Blocks:        []replay.Block{{Start: 0, End: len("> Thinking\n\n"), Part: part}},
		},
	); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, text, nativeID string
		calls                []openai.ToolCall
		restore              bool
	}{
		{name: "exact reply", text: visible, calls: calls, restore: true},
		{name: "edited text", text: visible + " edited", calls: calls},
		{name: "changed calls", text: visible},
		{
			name:     "native conversation differs",
			text:     visible,
			nativeID: "native-conversation",
			calls:    calls,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name,
			func(t *testing.T) {
				raw := replyTextJSON(t, test.text+ref.Marker())
				req := openai.ChatRequest{
					Messages: []openai.ChatMessage{
						{Role: "user", Content: raw},
						{Role: "assistant", Content: raw, ToolCalls: test.calls},
					},
				}
				next, err := NewIngress().PrepareReplay(
					t.Context(),
					slog.New(slog.NewTextHandler(io.Discard, nil)),
					store,
					test.nativeID,
					&req,
				)
				if err != nil {
					t.Fatal(err)
				}

				wantConversationID := ref.ConversationID
				if test.nativeID != "" {
					wantConversationID = test.nativeID
				}

				if next.ConversationID != wantConversationID || next.ReplyID == ref.ReplyID {
					t.Fatalf("new reference = %+v", next)
				}

				if string(req.Messages[0].Content) != string(raw) {
					t.Fatal("user text changed")
				}

				reply := req.Messages[1]
				if string(reply.Content) != string(replyTextJSON(t, test.text)) {
					t.Fatal("assistant reference did not strip")
				}

				if test.restore {
					if len(reply.ReplayParts) != 2 || reply.ReplayParts[0] != part {
						t.Fatalf("unexpected replay parts: %+v", reply.ReplayParts)
					}
				} else if len(
					reply.ReplayParts,
				) != 1 || reply.ReplayParts[0].Kind != render.SyntheticKindText {
					t.Fatalf("expected visible text only: %+v", reply.ReplayParts)
				}
			},
		)
	}
}

func TestPrepareReplayImportsLegacyReply(t *testing.T) {
	t.Parallel()

	store := replay.Store{Path: filepath.Join(t.TempDir(), "reply.db")}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	legacy := render.FormatSyntheticContentDeltaWithRef(
		render.SyntheticKindThinking,
		true,
		true,
		"rs_test",
		render.OriginCodex,
		"draft",
	) + render.SyntheticContentCloseWithAttrs(
		render.SyntheticKindThinking,
		"ciphertext",
		"",
	) + "Answer"

	for range 2 {
		req := openai.ChatRequest{
			Messages: []openai.ChatMessage{{Role: "assistant", Content: replyTextJSON(t, legacy)}},
		}
		_, err := NewIngress().PrepareReplay(t.Context(), log, store, "", &req)
		if err != nil {
			t.Fatal(err)
		}

		parts := req.Messages[0].ReplayParts
		if len(parts) != 2 || parts[0].Encrypted != "ciphertext" || parts[0].Ref != "rs_test" {
			t.Fatalf("unexpected imported parts: %+v", parts)
		}
	}

	_, found, err := store.Load(
		t.Context(),
		replay.Reference{
			ConversationID: "clyde-legacy",
			ReplyID:        replay.Fingerprint(legacy, nil),
		},
	)
	if err != nil || !found {
		t.Fatalf("lookup legacy reply: found=%v, error=%v", found, err)
	}
}

func TestPrepareReplayResumesToolOnlyReply(t *testing.T) {
	t.Parallel()

	store := replay.Store{Path: filepath.Join(t.TempDir(), "reply.db")}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := openai.ChatRequest{
		Messages: []openai.ChatMessage{{Role: "user", Content: replyTextJSON(t, "inspect")}},
	}
	first, err := NewIngress().PrepareReplay(t.Context(), log, store, "", &req)
	if err != nil {
		t.Fatal(err)
	}

	other, err := NewIngress().PrepareReplay(t.Context(), log, store, "", &req)
	if err != nil {
		t.Fatal(err)
	}

	if first.ConversationID == other.ConversationID {
		t.Fatal("chats with identical opening messages received the same conversation ID")
	}

	calls := []openai.ToolCall{
		{
			ID:       "call_test",
			Function: openai.ToolCallFunction{Name: "Read", Arguments: `{"path":"test"}`},
		},
	}
	err = store.Save(
		t.Context(),
		replay.Record{
			Reference:     first,
			Provider:      "codex",
			Digest:        replay.Fingerprint("", calls),
			VisibleDigest: replay.TextFingerprint(""),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	req.Messages = append(
		req.Messages,
		openai.ChatMessage{
			Role:      "assistant",
			Content:   replyTextJSON(t, strings.TrimSpace(first.Marker())),
			ToolCalls: calls,
		},
	)

	next, err := NewIngress().PrepareReplay(t.Context(), log, store, "", &req)
	if err != nil {
		t.Fatal(err)
	}

	if next.ConversationID != first.ConversationID || next.ReplyID == first.ReplyID {
		t.Fatalf("continuation reference = %+v", next)
	}

	if msg := req.Messages[1]; string(msg.Content) != `""` || len(msg.ReplayParts) != 0 ||
		len(msg.ToolCalls) != 1 {
		t.Fatalf("unexpected tool-only history: %+v", msg)
	}
}

func replyTextJSON(t *testing.T, text string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}
