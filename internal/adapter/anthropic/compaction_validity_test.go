package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The Anthropic Messages API enforces three structural rules on a request that
// the split's truncation can break. Each was measured against the live API on
// 2026-09-19 with a real Claude Code compaction:
//
//	messages.1: role 'system' must precede an 'assistant' message or end the
//	array
//
//	messages.0.content.4: unexpected `tool_use_id` found in `tool_result`
//	blocks: toolu_01JW3ja39xg7Y5EbENiTgJEC. Each `tool_result` block must have
//	a corresponding `tool_use` block in the previous message.
//
// A message with an empty content array is rejected the same way, so the
// truncation drops a message it emptied.
//
// assertRequestValid applies all three to a rewritten body. A test that builds
// a request the API would refuse fails here rather than in a live run.
func assertRequestValid(t *testing.T, body []byte) {
	t.Helper()
	messages := decodeShape(t, body)
	for index, message := range messages {
		if len(message.Blocks) == 0 && message.PlainText == "" {
			t.Errorf("messages.%d has no content: %s", index, shapeOf(messages))
		}
		if message.Role == "system" {
			following := "end of array"
			if index+1 < len(messages) {
				following = messages[index+1].Role
			}
			if index+1 < len(messages) && following != "assistant" {
				t.Errorf("messages.%d is a system message followed by %q: %s",
					index, following, shapeOf(messages))
			}
		}
		for blockIndex, block := range message.Blocks {
			if block.Type != "tool_result" {
				continue
			}
			if index == 0 || !messages[index-1].calls(block.ToolUseID) {
				t.Errorf("messages.%d.content.%d answers %s, which the previous message does not call: %s",
					index, blockIndex, block.ToolUseID, shapeOf(messages))
			}
		}
		for _, block := range message.Blocks {
			if block.Type != "tool_use" {
				continue
			}
			if index+1 >= len(messages) || !messages[index+1].answers(block.ID) {
				t.Errorf("messages.%d calls %s, which the next message does not answer: %s",
					index, block.ID, shapeOf(messages))
			}
		}
	}
}

type shapeMessage struct {
	Role      string
	PlainText string
	Blocks    []shapeBlock
}

type shapeBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
}

func (m shapeMessage) calls(id string) bool {
	for _, block := range m.Blocks {
		if block.Type == "tool_use" && block.ID == id {
			return true
		}
	}
	return false
}

func (m shapeMessage) answers(id string) bool {
	for _, block := range m.Blocks {
		if block.Type == "tool_result" && block.ToolUseID == id {
			return true
		}
	}
	return false
}

func decodeShape(t *testing.T, body []byte) []shapeMessage {
	t.Helper()
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode rewritten request: %v", err)
	}
	out := make([]shapeMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		shape := shapeMessage{Role: message.Role, PlainText: "", Blocks: nil}
		if err := json.Unmarshal(message.Content, &shape.Blocks); err != nil {
			if err := json.Unmarshal(message.Content, &shape.PlainText); err != nil {
				t.Fatalf("decode content of a %s message: %v", message.Role, err)
			}
		}
		out = append(out, shape)
	}
	return out
}

func shapeOf(messages []shapeMessage) string {
	var out strings.Builder
	for index, message := range messages {
		fmt.Fprintf(&out, "\n  [%d] %s:", index, message.Role)
		if len(message.Blocks) == 0 {
			out.WriteString(" " + strings.TrimSpace(message.PlainText))
			continue
		}
		for _, block := range message.Blocks {
			out.WriteString(" " + block.Type)
			if block.ID != "" {
				out.WriteString("(" + block.ID + ")")
			}
			if block.ToolUseID != "" {
				out.WriteString("(for " + block.ToolUseID + ")")
			}
		}
	}
	return out.String()
}

// toolPairFixture is the shape Claude Code sends: a system reminder between
// turns, an assistant tool call, and the matching result in the next message.
const toolPairFixture = `{
  "model": "claude-opus-5",
  "messages": [
    {"role": "user", "content": "start"},
    {"role": "assistant", "content": [
      {"type": "text", "text": "reading the tests"},
      {"type": "tool_use", "id": "toolu_01AA", "name": "Bash",
       "input": {"command": "go test ./..."}}]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_01AA", "content": "ok  42 tests"}]},
    {"role": "system", "content": [
      {"type": "text", "text": "<system-reminder>stay on task</system-reminder>"}]},
    {"role": "assistant", "content": [
      {"type": "tool_use", "id": "toolu_01BB", "name": "Bash",
       "input": {"command": "go vet ./..."}}]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_01BB", "content": "clean"}]},
    {"role": "user", "content": [
      {"type": "text", "text": "Your task is to create a detailed summary"}]}
  ]
}`

// TestTruncateCompactionRequestStaysValidAtEveryCut cuts the same conversation
// at every message and segment and asserts the API would accept each result.
// The live run found two rejections this covers, one where a system reminder
// ended the kept prefix and one where a tool result outlived its call.
func TestTruncateCompactionRequestStaysValidAtEveryCut(t *testing.T) {
	t.Parallel()
	decoded, err := DecodeCompactionRequest([]byte(toolPairFixture))
	if err != nil {
		t.Fatalf("DecodeCompactionRequest: %v", err)
	}
	const instructionStart = 6
	for messageIndex := 1; messageIndex < instructionStart; messageIndex++ {
		for segmentIndex := range decoded.Messages[messageIndex].Segments {
			for _, headRunes := range []int{0, 4} {
				cut := CompactionCut{
					MessageIndex: messageIndex,
					SegmentIndex: segmentIndex,
					HeadRunes:    headRunes,
				}
				out, truncateErr := TruncateCompactionRequest(
					[]byte(toolPairFixture), cut, instructionStart)
				if truncateErr != nil {
					t.Errorf("cut %+v: %v", cut, truncateErr)
					continue
				}
				t.Run(fmt.Sprintf("message%d_segment%d_head%d", messageIndex, segmentIndex, headRunes),
					func(t *testing.T) {
						assertRequestValid(t, out)
					})
			}
		}
	}
}
