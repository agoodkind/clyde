package parser

import (
	"encoding/json"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
	"goodkind.io/clyde/internal/transcript"
)

type composerToolStatus string

const (
	composerToolStatusError     composerToolStatus = "error"
	composerToolStatusFailed    composerToolStatus = "failed"
	composerToolStatusCancelled composerToolStatus = "cancelled"
)

func mapJSONLMessage(
	message cursorjsonl.TranscriptMessage,
	_ conversation.LoadOptions,
) (transcript.Message, bool) {
	textParts := make([]string, 0)
	tools := make([]transcript.ToolCall, 0)
	for _, part := range message.Parts {
		switch part.Type {
		case cursorjsonl.PartTypeText:
			if strings.TrimSpace(part.Text) != "" {
				textParts = append(textParts, part.Text)
			}
		case cursorjsonl.PartTypeToolUse:
			tools = append(tools, transcript.ToolCall{
				ID:      "",
				Name:    part.ToolName,
				Input:   transcript.ToolInputJSON{Raw: cloneRaw(part.ToolInput)},
				Output:  "",
				IsError: false,
			})
		}
	}
	text := strings.Join(textParts, "\n")
	if text == "" && len(tools) == 0 {
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              message.Role,
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              text,
		Thinking:          "",
		HasTools:          len(tools) > 0,
		Tools:             tools,
	}, true
}

func mapComposerBubble(
	bubble cursorstore.Bubble,
	opts conversation.LoadOptions,
) (transcript.Message, bool) {
	role := "assistant"
	if bubble.Type == cursorstore.BubbleTypeUser {
		role = "user"
	}
	tools := make([]transcript.ToolCall, 0)
	if bubble.ToolCall != nil {
		tools = append(tools, transcript.ToolCall{
			ID:      "",
			Name:    bubble.ToolCall.Name,
			Input:   transcript.ToolInputJSON{Raw: rawArgsToJSON(bubble.ToolCall.RawArgs)},
			Output:  outputIf(opts.IncludeToolOutputs, bubble.ToolCall.Result),
			IsError: toolErrored(bubble.ToolCall.Status),
		})
	}
	if bubble.Text == "" && bubble.Thinking.Text == "" && len(tools) == 0 {
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID:              bubble.BubbleID,
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              role,
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              bubble.Text,
		Thinking:          bubble.Thinking.Text,
		HasTools:          len(tools) > 0,
		Tools:             tools,
	}, true
}

func mapLegacyBubble(
	bubble cursorstore.LegacyChatBubble,
	_ conversation.LoadOptions,
) (transcript.Message, bool) {
	role := "assistant"
	if bubble.Type == cursorstore.LegacyChatRoleUser {
		role = "user"
	}
	if bubble.Text == "" {
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              role,
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              bubble.Text,
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}, true
}

func emptyMessage() transcript.Message {
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "",
		Visibility:        "",
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              "",
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func rawArgsToJSON(raw string) json.RawMessage {
	trimmed := strings.TrimSpace(raw)
	if json.Valid([]byte(trimmed)) {
		return append(json.RawMessage(nil), []byte(trimmed)...)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	return encoded
}

func toolErrored(status string) bool {
	normalizedStatus := composerToolStatus(strings.ToLower(strings.TrimSpace(status)))
	switch normalizedStatus {
	case composerToolStatusError, composerToolStatusFailed, composerToolStatusCancelled:
		return true
	default:
		return false
	}
}

func outputIf(include bool, output string) string {
	if include {
		return output
	}
	return ""
}
