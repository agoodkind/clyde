package anthropicbackend

import (
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// TestTranslateRequestAssistantRedactedThinkingMaterializesNativeBlock
// asserts that when Cursor replays an assistant text containing a
// `<!--clyde-redacted-thinking-->...<!--/clyde-redacted-thinking
// data-encrypted="..."-->` envelope, the Anthropic mapper materializes
// it into one AnthContentBlock with Type="redacted_thinking" carrying
// the opaque blob in Data under MaterializeNativeThinkingBlock, and
// drops the envelope entirely under MaterializeDrop. This is the
// inbound contract that pairs with CLYDE-262 step 6 in mapper_impl.go.
func TestTranslateRequestAssistantRedactedThinkingMaterializesNativeBlock(t *testing.T) {
	t.Parallel()
	envelope := adapterrender.SyntheticContentOpen(adapterrender.SyntheticRedactedThinking) +
		"> [redacted]" +
		adapterrender.SyntheticContentCloseWithAttrs(
			adapterrender.SyntheticRedactedThinking,
			"REDACTED_BLOB_BASE64==",
			"",
		)
	assistantText := envelope + "Final answer."
	body, err := json.Marshal(assistantText)
	if err != nil {
		t.Fatal(err)
	}
	buildReq := func() adapteropenai.ChatRequest {
		return adapteropenai.ChatRequest{
			Model: "x",
			Messages: []adapteropenai.ChatMessage{
				{Role: "user", Content: json.RawMessage(`"q"`)},
				{Role: "assistant", Content: json.RawMessage(body)},
				{Role: "user", Content: json.RawMessage(`"continue"`)},
			},
		}
	}

	t.Run("native_thinking_block_emits_redacted_block", func(t *testing.T) {
		out, err := TranslateRequest(buildReq(), "", 64, adapterrender.MaterializeNativeThinkingBlock)
		if err != nil {
			t.Fatal(err)
		}
		asst := out.Messages[1]
		var redacted []AnthContentBlock
		var textBlocks []AnthContentBlock
		for _, blk := range asst.Content {
			switch blk.Type {
			case "redacted_thinking":
				redacted = append(redacted, blk)
			case "text":
				textBlocks = append(textBlocks, blk)
			}
		}
		if len(redacted) != 1 {
			t.Fatalf("redacted_thinking block count=%d want 1: %+v", len(redacted), asst.Content)
		}
		if redacted[0].Data != "REDACTED_BLOB_BASE64==" {
			t.Fatalf("redacted_thinking.Data=%q want REDACTED_BLOB_BASE64==", redacted[0].Data)
		}
		if redacted[0].Thinking != "" {
			t.Fatalf("redacted_thinking.Thinking=%q want empty", redacted[0].Thinking)
		}
		if len(textBlocks) == 0 || !strings.Contains(textBlocks[0].Text, "Final answer.") {
			t.Fatalf("expected trailing text block with Final answer, got %+v", asst.Content)
		}
	})

	t.Run("drop_strategy_emits_zero_redacted_blocks", func(t *testing.T) {
		out, err := TranslateRequest(buildReq(), "", 64, adapterrender.MaterializeDrop)
		if err != nil {
			t.Fatal(err)
		}
		asst := out.Messages[1]
		for _, blk := range asst.Content {
			if blk.Type == "redacted_thinking" {
				t.Fatalf("drop strategy must not emit redacted_thinking: %+v", asst.Content)
			}
			if strings.Contains(blk.Text, "clyde-redacted-thinking") {
				t.Fatalf("drop strategy leaked envelope text: %+v", blk)
			}
		}
	})
}

// TestToAPIRequestRedactedThinkingEmitsDataField asserts that the wire
// shape produced by ToAPIRequest for a backend block of
// Type="redacted_thinking" with Data="blob" includes the canonical
// Anthropic JSON keys `"type":"redacted_thinking"` and `"data":"blob"`.
// This is the egress contract that pairs with CLYDE-262 step 7 in
// internal/adapter/anthropic/types.go and wire_helpers.go.
func TestToAPIRequestRedactedThinkingEmitsDataField(t *testing.T) {
	t.Parallel()
	req := AnthRequest{
		Model: "claude-test",
		Messages: []AnthMessage{
			{
				Role: "assistant",
				Content: []AnthContentBlock{
					{Type: "redacted_thinking", Data: "blob"},
					{Type: "text", Text: "answer"},
				},
			},
		},
		MaxTokens: 32,
	}
	out, _ := ToAPIRequest(req, "claude-test", false)
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)
	if !strings.Contains(body, `"type":"redacted_thinking"`) {
		t.Fatalf("missing redacted_thinking type in wire payload: %s", body)
	}
	if !strings.Contains(body, `"data":"blob"`) {
		t.Fatalf("missing data field in wire payload: %s", body)
	}
	// Defensive check: the resulting block in the typed model carries
	// the data on the public ContentBlock.Data field so future wire
	// transforms can read it without re-parsing JSON.
	asst := out.Messages[0]
	var redacted *anthropic.ContentBlock
	for i, blk := range asst.Content {
		if blk.Type == "redacted_thinking" {
			redacted = &asst.Content[i]
			break
		}
	}
	if redacted == nil {
		t.Fatalf("typed assistant content missing redacted_thinking block: %+v", asst.Content)
	}
	if redacted.Data != "blob" {
		t.Fatalf("typed Data=%q want blob", redacted.Data)
	}
}
