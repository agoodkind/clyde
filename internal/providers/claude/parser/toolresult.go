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
		tools := messages[i].Tools
		for j := range tools {
			tool := &tools[j]
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
		tool.Output = block.Content.SearchableText()
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

// toolResultBlock is one tool_result block inside a user entry's content. Its
// own content is a union: a tool returns its result as a string, as a list of
// content blocks, or in a shape this parser does not model, so it decodes
// through [ToolUseResultContent] rather than as a bare string. Declaring it as a
// string fails the whole entry when any one result is a list, taking the results
// beside it down with it.
type toolResultBlock struct {
	Type      string               `json:"type"`
	ToolUseID string               `json:"tool_use_id"`
	Content   ToolUseResultContent `json:"content"`
	IsError   bool                 `json:"is_error"`
}
