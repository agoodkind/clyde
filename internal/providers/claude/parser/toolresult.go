package parser

import (
	"bufio"
	"bytes"
	"encoding/json"

	"goodkind.io/clyde/internal/transcript"
)

// attachToolOutputs scans the transcript body once and fills each tool call's
// Output and IsError from the matching tool_result block in a later user entry.
func attachToolOutputs(body []byte, messages []transcript.Message) {
	toolsByID := make(map[string]*transcript.ToolCall)
	for i := range messages {
		for j := range messages[i].Tools {
			tool := &messages[i].Tools[j]
			if tool.ID != "" {
				toolsByID[tool.ID] = tool
			}
		}
	}
	if len(toolsByID) == 0 {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), scanBufferMax)
	for scanner.Scan() {
		attachToolOutputsFromLine(scanner.Bytes(), toolsByID)
	}
}

func attachToolOutputsFromLine(line []byte, toolsByID map[string]*transcript.ToolCall) {
	var entry toolResultEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return
	}
	if entry.Type != "user" || len(entry.Message.Content) == 0 {
		return
	}
	var blocks []toolResultBlock
	if err := json.Unmarshal(entry.Message.Content, &blocks); err != nil {
		return
	}
	for _, block := range blocks {
		tool := toolsByID[block.ToolUseID]
		if block.Type != "tool_result" || tool == nil {
			continue
		}
		tool.Output = block.Content
		tool.IsError = block.IsError
	}
}

type toolResultEntry struct {
	Type    string            `json:"type"`
	Message toolResultMessage `json:"message"`
}

type toolResultMessage struct {
	Content json.RawMessage `json:"content"`
}

type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}
