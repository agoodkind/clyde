package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// compactionFixture is one realistic compaction request. Block shapes match
// capture row 361176.
const compactionFixture = `{
  "model": "claude-opus-5",
  "max_tokens": 4096,
  "metadata": {"user_id": "{\"session_id\":\"7eeb65ed-1034-4901-b744-7d55ed4a3042\"}"},
  "messages": [
    {"role": "user", "content": "start"},
    {"role": "assistant", "content": [
      {"type": "thinking", "thinking": "weighing the options", "signature": "sig-abc"},
      {"type": "text", "text": "a long file dump"}]},
    {"role": "assistant", "content": [
      {"type": "tool_use", "id": "toolu_01FT", "name": "Bash",
       "input": {"command": "go test ./...", "description": "run the tests"}}]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_01FT", "content": "ok  42 tests"}]},
    {"role": "user", "content": [
      {"type": "text", "text": "Your task is to create a detailed summary"}]}
  ]
}`

func TestDecodeCompactionRequestClassifiesEveryBlock(t *testing.T) {
	t.Parallel()
	request, err := DecodeCompactionRequest([]byte(compactionFixture))
	if err != nil {
		t.Fatalf("DecodeCompactionRequest: %v", err)
	}
	if request.SessionID != "7eeb65ed-1034-4901-b744-7d55ed4a3042" {
		t.Errorf("session id = %q", request.SessionID)
	}
	if len(request.Messages) != 5 {
		t.Fatalf("messages = %d, want 5", len(request.Messages))
	}
	assistant := request.Messages[1]
	if len(assistant.Segments) != 2 {
		t.Fatalf("assistant segments = %d, want 2", len(assistant.Segments))
	}
	if assistant.Segments[0].Kind != SegmentThinking {
		t.Errorf("first segment kind = %v, want thinking", assistant.Segments[0].Kind)
	}
	if assistant.Segments[0].Text != "weighing the options" {
		t.Errorf("thinking text = %q", assistant.Segments[0].Text)
	}
	if assistant.Segments[1].Kind != SegmentText {
		t.Errorf("second segment kind = %v, want text", assistant.Segments[1].Kind)
	}

	call := request.Messages[2].Segments[0]
	if call.Kind != SegmentToolUse || call.ToolUseID != "toolu_01FT" {
		t.Errorf("tool call segment = %+v", call)
	}
	if !strings.Contains(call.Text, "go test ./...") {
		t.Errorf("tool call text omits the input: %q", call.Text)
	}
	result := request.Messages[3].Segments[0]
	if result.Kind != SegmentToolResult || result.ToolUseID != "toolu_01FT" {
		t.Errorf("tool result segment = %+v", result)
	}
}

func TestDecodeCompactionRequestRejectsMalformedBody(t *testing.T) {
	t.Parallel()
	if _, err := DecodeCompactionRequest([]byte("{not json")); err == nil {
		t.Fatal("expected an error for a malformed body")
	}
}

// decodeMessages returns the messages array of a rewritten request body.
func decodeMessages(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var top struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	return top.Messages
}

func TestTruncateCompactionRequestCutsInsideATextBlock(t *testing.T) {
	t.Parallel()
	cut := CompactionCut{MessageIndex: 1, SegmentIndex: 1, HeadRunes: 6}
	out, err := TruncateCompactionRequest([]byte(compactionFixture), cut, 4)
	if err != nil {
		t.Fatalf("TruncateCompactionRequest: %v", err)
	}
	messages := decodeMessages(t, out)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(messages))
	}
	var blocks []map[string]string
	if err := json.Unmarshal(messages[1]["content"], &blocks); err != nil {
		t.Fatalf("decode boundary blocks: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("boundary blocks = %d, want 2", len(blocks))
	}
	if blocks[0]["signature"] != "sig-abc" {
		t.Errorf("thinking block lost its signature: %v", blocks[0])
	}
	if blocks[1]["text"] != "a long" {
		t.Errorf("text block = %q, want %q", blocks[1]["text"], "a long")
	}

	var model string
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("decode rewritten top level: %v", err)
	}
	if err := json.Unmarshal(top["model"], &model); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if model != "claude-opus-5" {
		t.Errorf("model = %q", model)
	}
	if string(top["max_tokens"]) != "4096" {
		t.Errorf("max_tokens = %s", top["max_tokens"])
	}
}

func TestTruncateCompactionRequestAnswersAnOrphanedToolCall(t *testing.T) {
	t.Parallel()
	cut := CompactionCut{MessageIndex: 2, SegmentIndex: 0, HeadRunes: 8}
	out, err := TruncateCompactionRequest([]byte(compactionFixture), cut, 4)
	if err != nil {
		t.Fatalf("TruncateCompactionRequest: %v", err)
	}
	request, err := DecodeCompactionRequest(out)
	if err != nil {
		t.Fatalf("DecodeCompactionRequest on the rewritten body: %v", err)
	}
	calls := 0
	results := 0
	for _, message := range request.Messages {
		for _, segment := range message.Segments {
			switch segment.Kind {
			case SegmentToolUse:
				calls++
			case SegmentToolResult:
				results++
			}
		}
	}
	if calls != results {
		t.Fatalf("tool calls = %d, tool results = %d", calls, results)
	}
	if calls == 0 {
		t.Fatal("the rewritten request retained no tool call")
	}
}

func TestTruncateCompactionRequestKeepsToolInputAnObject(t *testing.T) {
	t.Parallel()
	cut := CompactionCut{MessageIndex: 2, SegmentIndex: 0, HeadRunes: 8}
	out, err := TruncateCompactionRequest([]byte(compactionFixture), cut, 4)
	if err != nil {
		t.Fatalf("TruncateCompactionRequest: %v", err)
	}
	messages := decodeMessages(t, out)
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(messages[2]["content"], &blocks); err != nil {
		t.Fatalf("decode tool call blocks: %v", err)
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(blocks[0]["input"], &input); err != nil {
		t.Fatalf("tool input is not a JSON object: %s", blocks[0]["input"])
	}
}

func TestTruncateCompactionRequestDropsAnEmptyBoundaryMessage(t *testing.T) {
	t.Parallel()
	cut := CompactionCut{MessageIndex: 2, SegmentIndex: 0, HeadRunes: 0}
	out, err := TruncateCompactionRequest([]byte(compactionFixture), cut, 4)
	if err != nil {
		t.Fatalf("TruncateCompactionRequest: %v", err)
	}
	messages := decodeMessages(t, out)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(messages))
	}
}
