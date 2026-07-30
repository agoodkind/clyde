package parser

import (
	"goodkind.io/clyde/internal/transcript"
)

// attachToolResults fills each tool call's Output and IsError from the result
// that answered it.
//
// The results are collected while the transcript is parsed, so the file is read
// once and each content array is decoded once. A result whose call is absent is
// dropped: that call belongs to a turn the load options excluded.
func attachToolResults(messages []transcript.Message, results []claudeToolResult) {
	if len(results) == 0 {
		return
	}
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
	for _, result := range results {
		tool := toolsByID[result.ToolUseID]
		if tool == nil {
			continue
		}
		tool.Output = result.Output
		tool.IsError = result.IsError
	}
}
