package anthropicbackend

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToAPIRequestCopiesThinkingBody asserts that ToAPIRequest copies the
// AnthContentBlock.Thinking body into the wire ContentBlock and that the
// resulting JSON carries `"thinking":"<body>"`. Without this copy, Anthropic
// rejects the request with `messages.N.content.M.thinking.thinking: Field
// required`.
func TestToAPIRequestCopiesThinkingBody(t *testing.T) {
	t.Parallel()
	tr := AnthRequest{
		Messages: []AnthMessage{{
			Role: "assistant",
			Content: []AnthContentBlock{
				{Type: "thinking", Thinking: "deliberation body"},
				{Type: "text", Text: "final"},
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
	if wireBlocks[0].Type != "thinking" {
		t.Fatalf("block0 type=%q want thinking", wireBlocks[0].Type)
	}
	if wireBlocks[0].Thinking != "deliberation body" {
		t.Fatalf("block0 thinking=%q want %q", wireBlocks[0].Thinking, "deliberation body")
	}
	encoded, err := json.Marshal(wireBlocks[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"thinking":"deliberation body"`) {
		t.Fatalf("thinking body missing from wire JSON: %s", encoded)
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
				{Type: "thinking", Thinking: ""},
				{Type: "thinking", Thinking: "   "},
				{Type: "text", Text: "answer"},
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
