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

// TestRequestSamplingFieldsSerializeWhenSet asserts the optional
// sampling and stop-sequence knobs land on the wire under their
// Anthropic key names when the caller sets them.
func TestRequestSamplingFieldsSerializeWhenSet(t *testing.T) {
	t.Parallel()
	temperature := 0.4
	topP := 0.9
	req := Request{
		Model:         "claude-x",
		Messages:      []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens:     16,
		Temperature:   &temperature,
		TopP:          &topP,
		StopSequences: []string{"STOP"},
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"temperature":0.4`) {
		t.Fatalf("expected temperature in wire JSON, got %q", got)
	}
	if !strings.Contains(got, `"top_p":0.9`) {
		t.Fatalf("expected top_p in wire JSON, got %q", got)
	}
	if !strings.Contains(got, `"stop_sequences":["STOP"]`) {
		t.Fatalf("expected stop_sequences in wire JSON, got %q", got)
	}
}

// TestRequestSamplingFieldsAbsentOmitsKeys asserts the additive,
// present-only contract: with no sampling or stop set, the marshaled
// request carries none of the three keys.
func TestRequestSamplingFieldsAbsentOmitsKeys(t *testing.T) {
	t.Parallel()
	req := Request{
		Model:     "claude-x",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 16,
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(out)
	for _, key := range []string{"temperature", "top_p", "stop_sequences"} {
		if strings.Contains(got, key) {
			t.Fatalf("absent sampling field %q leaked into wire JSON: %q", key, got)
		}
	}
}

// TestRequestSamplingFieldsRoundTrip asserts the UnmarshalJSON mirror
// decodes the three fields back into the typed Request.
func TestRequestSamplingFieldsRoundTrip(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"model":"claude-x","messages":[],"max_tokens":16,"temperature":0.4,"top_p":0.9,"stop_sequences":["a","b"]}`)
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Temperature == nil || *req.Temperature != 0.4 {
		t.Fatalf("Temperature = %v want 0.4", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.9 {
		t.Fatalf("TopP = %v want 0.9", req.TopP)
	}
	if len(req.StopSequences) != 2 || req.StopSequences[0] != "a" || req.StopSequences[1] != "b" {
		t.Fatalf("StopSequences = %v want [a b]", req.StopSequences)
	}
}
