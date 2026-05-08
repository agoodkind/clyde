package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestContentBlockThinkingFieldSerializes anchors the Anthropic wire contract
// for the thinking content block. Anthropic rejects a thinking block whose
// body is missing with `messages.N.content.M.thinking.thinking: Field required`,
// so the wire ContentBlock must serialize the Thinking field as a top-level
// "thinking" key alongside "type":"thinking".
func TestContentBlockThinkingFieldSerializes(t *testing.T) {
	t.Parallel()
	block := ContentBlock{Type: "thinking", Thinking: "x"}
	out, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"thinking":"x"`) {
		t.Fatalf("expected thinking body in wire JSON, got %q", got)
	}
	if !strings.Contains(got, `"type":"thinking"`) {
		t.Fatalf("expected type=thinking in wire JSON, got %q", got)
	}
}
