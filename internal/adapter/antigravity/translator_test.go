package antigravity_test

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/antigravity"
	"goodkind.io/clyde/internal/adapter/cursor"
	"goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/replay"
	"goodkind.io/clyde/internal/adapter/resolver"
)

func TestGeminiToolSignatureRoundTrip(t *testing.T) {
	const signature = "fixture-opaque-signature"
	const stream = "data: " +
		`{"response":{"candidates":[{"content":{"parts":[{"functionCall":` +
		`{"name":"lookup","args":{"key":1}},"thoughtSignature":"` + signature + `"}]}}],` +
		`"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":12}}}`

	var events streamRecorder
	result, err := antigravity.ReadStream(t.Context(), strings.NewReader(stream), &events)
	if err != nil {
		t.Fatal(err)
	}

	details := result.Usage.PromptTokensDetails
	reasoning := result.Usage.CompletionTokensDetails
	if details == nil || details.CachedTokens != 3 ||
		details.CacheWriteTokens == nil || *details.CacheWriteTokens != 0 ||
		reasoning == nil || reasoning.ReasoningTokens != 0 {
		t.Fatalf("Gemini usage omitted zero-valued details: %+v", result.Usage)
	}

	logger := slog.New(slog.DiscardHandler)
	renderer := render.NewEventRenderer("request", "gemini-fixture", "antigravity", logger)
	reply := renderer.CaptureReply()

	for _, event := range events {
		chunks := renderer.HandleEvent(event)
		wire, err := json.Marshal(chunks)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(string(wire), signature) {
			t.Fatal("signature appeared in the OpenAI response")
		}
	}

	calls := reply.ToolCalls()
	if len(calls) != 1 || calls[0].ID == "" || len(reply.Blocks) != 1 ||
		reply.Blocks[0].Part.Ref != calls[0].ID || reply.Blocks[0].Part.Signature != signature {
		t.Fatal("renderer did not associate the signature with the generated tool call ID")
	}

	ref := replay.Reference{ConversationID: "fixture-conversation", ReplyID: "fixture-reply"}
	store := replay.Store{Path: filepath.Join(t.TempDir(), "replay.db")}
	if err := store.Save(
		t.Context(),
		replay.Record{
			Reference:     ref,
			Provider:      "antigravity",
			Digest:        replay.Fingerprint(reply.Text(), calls),
			VisibleDigest: replay.TextFingerprint(reply.Text()),
			ToolDigest:    replay.ToolFingerprint(calls),
			Blocks:        []replay.Block{replay.Block(reply.Blocks[0])},
		},
	); err != nil {
		t.Fatal(err)
	}

	for _, conversation := range []string{ref.ConversationID, "another-conversation"} {
		request := openai.ChatRequest{
			Messages: []openai.ChatMessage{
				{Role: "assistant", ToolCalls: calls},
				{Role: "tool", ToolCallID: calls[0].ID, Content: json.RawMessage(`"result"`)},
			},
		}
		if _, err := cursor.NewIngress().PrepareReplay(
			t.Context(),
			logger,
			store,
			conversation,
			&request,
		); err != nil {
			t.Fatal(err)
		}

		body, err := antigravity.BuildGenerateContent(
			resolver.ResolvedRequest{Model: "gemini-fixture", OpenAI: request},
			false,
		)
		if err != nil {
			t.Fatal(err)
		}

		wire, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}

		gotSignature := strings.Contains(string(wire), `"thoughtSignature":"`+signature+`"`)
		if gotSignature != (conversation == ref.ConversationID) {
			t.Fatalf("signature present = %v for conversation %q", gotSignature, conversation)
		}
	}
}

func TestGeminiRequestReplaysForeignReasoningAsText(t *testing.T) {
	request := resolver.ResolvedRequest{
		Model: "gemini-fixture",
		OpenAI: openai.ChatRequest{
			Messages: []openai.ChatMessage{
				{
					Role: "assistant",
					ReplayParts: []render.SyntheticPart{
						{
							Kind:      render.SyntheticKindThinking,
							Body:      "foreign reasoning",
							Origin:    render.OriginAnthropic,
							Signature: "foreign-signature",
						},
						{Kind: render.SyntheticKindText, Body: "answer"},
					},
				},
			},
		},
	}
	body, err := antigravity.BuildGenerateContent(request, false)
	if err != nil {
		t.Fatal(err)
	}

	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	const want = `{"contents":[{"role":"model","parts":[{"text":"foreign reasoning"},{"text":"answer"}]}]}`
	if string(wire) != want {
		t.Fatalf("Gemini input = %s, want plain text replay", wire)
	}
}
