package anthropicbackend

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToAPIRequestInjectsUnsignedThinkingAsText asserts that ToAPIRequest
// refuses to emit a native thinking block without a signature. A readable
// body-only block is preserved as ordinary assistant text so prior thinking
// remains in context without crossing Anthropic's signature boundary.
func TestToAPIRequestInjectsUnsignedThinkingAsText(t *testing.T) {
	t.Parallel()
	tr := AnthRequest{
		Messages: []AnthMessage{{
			Role: "assistant",
			Content: []AnthContentBlock{
				ThinkingBlock{Thinking: "deliberation body"},
				TextBlock{Text: "final"},
			},
		}},
		MaxTokens: 64,
	}
	out, _ := ToAPIRequest(tr, "claude-x", false)
	if len(out.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(out.Messages))
	}
	wireBlocks := out.Messages[0].Content
	if len(wireBlocks) != 2 {
		t.Fatalf("wire blocks=%d want 2: %+v", len(wireBlocks), wireBlocks)
	}
	if wireBlocks[0].Type != "text" {
		t.Fatalf("block0 type=%q want text", wireBlocks[0].Type)
	}
	if wireBlocks[0].Text != "deliberation body" {
		t.Fatalf("block0 text=%q want %q", wireBlocks[0].Text, "deliberation body")
	}
	encoded, err := json.Marshal(wireBlocks[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wireJSON := string(encoded)
	if strings.Contains(wireJSON, `"type":"thinking"`) {
		t.Fatalf("unsigned thinking leaked as native wire block: %s", wireJSON)
	}
	if !strings.Contains(wireJSON, `"text":"deliberation body"`) {
		t.Fatalf("thinking body missing from text wire JSON: %s", encoded)
	}
}

// TestToAPIRequestDropsEmptyThinkingBlock asserts the defense-in-depth at the
// wire boundary: a thinking block whose body is empty or whitespace must not
// reach Anthropic, regardless of caller. The original 400 was triggered by
// exactly this shape, an empty-body `{type: thinking}` arriving on the wire.
func TestToAPIRequestDropsEmptyThinkingBlock(t *testing.T) {
	t.Parallel()
	tr := AnthRequest{
		Messages: []AnthMessage{{
			Role: "assistant",
			Content: []AnthContentBlock{
				ThinkingBlock{Thinking: ""},
				ThinkingBlock{Thinking: "   "},
				TextBlock{Text: "answer"},
			},
		}},
		MaxTokens: 64,
	}
	out, _ := ToAPIRequest(tr, "claude-x", false)
	wireBlocks := out.Messages[0].Content
	for _, b := range wireBlocks {
		if b.Type == "thinking" {
			t.Fatalf("empty-body thinking block leaked through: %+v", b)
		}
	}
	if len(wireBlocks) != 1 || wireBlocks[0].Type != "text" {
		t.Fatalf("expected single text block, got %+v", wireBlocks)
	}
}

// TestToAPIRequestCopiesThinkingSignature asserts that ToAPIRequest
// copies the AnthContentBlock.Signature value into the wire ContentBlock
// and the resulting JSON includes `"signature":"<value>"` alongside
// `"thinking":"<body>"`. Anthropic's signature_delta SSE event is the
// upstream source of this value; preserving it on replay is what passes
// signature validation.
func TestToAPIRequestCopiesThinkingSignature(t *testing.T) {
	t.Parallel()
	tr := AnthRequest{
		Messages: []AnthMessage{{
			Role: "assistant",
			Content: []AnthContentBlock{
				ThinkingBlock{Thinking: "deliberation body", Signature: "SIG_BLOB=="},
			},
		}},
		MaxTokens: 64,
	}
	out, _ := ToAPIRequest(tr, "claude-x", false)
	if len(out.Messages) != 1 || len(out.Messages[0].Content) != 1 {
		t.Fatalf("unexpected message shape: %+v", out.Messages)
	}
	block := out.Messages[0].Content[0]
	if block.Signature != "SIG_BLOB==" {
		t.Fatalf("wire block signature=%q want SIG_BLOB==", block.Signature)
	}
	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"signature":"SIG_BLOB=="`) {
		t.Fatalf("signature missing from wire JSON: %s", encoded)
	}
}

// TestToAPIRequestPreservesSignedEmptyBodyThinkingBlock asserts the
// regression guard for CLYDE-261: when a thinking content block has an
// empty body but a non-empty Signature, the wire-boundary guard must NOT
// drop it. A signed empty body is a legitimate replay shape Anthropic
// emits in some thinking sessions; suppressing it would cause signature
// validation to fail upstream. The pre-CLYDE-261 guard dropped any
// thinking block with an empty body without inspecting the signature.
func TestToAPIRequestPreservesSignedEmptyBodyThinkingBlock(t *testing.T) {
	t.Parallel()
	tr := AnthRequest{
		Messages: []AnthMessage{{
			Role: "assistant",
			Content: []AnthContentBlock{
				ThinkingBlock{Thinking: "", Signature: "SIGNED_EMPTY_BODY=="},
				TextBlock{Text: "final"},
			},
		}},
		MaxTokens: 64,
	}
	out, _ := ToAPIRequest(tr, "claude-x", false)
	wireBlocks := out.Messages[0].Content
	if len(wireBlocks) != 2 {
		t.Fatalf("wire blocks=%d want 2 (signed thinking + text); got %+v", len(wireBlocks), wireBlocks)
	}
	if wireBlocks[0].Type != "thinking" {
		t.Fatalf("block0 type=%q want thinking", wireBlocks[0].Type)
	}
	if wireBlocks[0].Signature != "SIGNED_EMPTY_BODY==" {
		t.Fatalf("block0 signature=%q want SIGNED_EMPTY_BODY==", wireBlocks[0].Signature)
	}
	encoded, err := json.Marshal(wireBlocks[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)
	if !strings.Contains(body, `"signature":"SIGNED_EMPTY_BODY=="`) {
		t.Fatalf("wire JSON missing signature: %s", body)
	}
	// Empty thinking body must NOT appear on the wire (omitempty).
	if strings.Contains(body, `"thinking":""`) {
		t.Fatalf("empty thinking body should be omitted: %s", body)
	}
}
