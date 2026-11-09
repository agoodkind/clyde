package parser

import (
	"strings"

	"goodkind.io/clyde/internal/conversation"
	zedstore "goodkind.io/clyde/internal/providers/zed/store"
	"goodkind.io/clyde/internal/transcript"
)

func transcriptMessage(
	thread zedstore.ThreadDocument,
	message zedstore.ThreadMessage,
	opts conversation.LoadOptions,
) (transcript.Message, bool) {
	defer transcript.ContainMappingPanic()
	switch message.Kind {
	case zedstore.ThreadMessageKindUser:
		if message.User == nil {
			return emptyMessage(), false
		}

		text := userMessageText(message.User)
		if strings.TrimSpace(text) == "" {
			return emptyMessage(), false
		}

		return transcript.Message{
			UUID:              message.User.ID,
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "user",
			Visibility:        transcript.MessageVisibilityVisible,
			Compaction:        nil,
			Timestamp:         thread.UpdatedAt,
			Text:              text,
			Thinking:          "",
			HasTools:          false,
			Tools:             nil,
			Attachments:       nil,
			TextParts:         nil,
		}, true
	case zedstore.ThreadMessageKindAgent:
		if message.Agent == nil {
			return emptyMessage(), false
		}

		text, thinking, tools := agentMessageParts(message.Agent, opts.IncludeToolOutputs)
		if text == "" && thinking == "" && len(tools) == 0 {
			return emptyMessage(), false
		}

		return transcript.Message{
			UUID:              "",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "assistant",
			Visibility:        transcript.MessageVisibilityVisible,
			Compaction:        nil,
			Timestamp:         thread.UpdatedAt,
			Text:              text,
			Thinking:          thinking,
			HasTools:          len(tools) > 0,
			Tools:             tools,
			Attachments:       nil,
			TextParts:         nil,
		}, true
	case zedstore.ThreadMessageKindResume:
		return transcript.Message{
			UUID:              "",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "user",
			Visibility:        transcript.MessageVisibilityVisible,
			Compaction:        nil,
			Timestamp:         thread.UpdatedAt,
			Text:              "Continue where you left off",
			Thinking:          "",
			HasTools:          false,
			Tools:             nil,
			Attachments:       nil,
			TextParts:         nil,
		}, true
	case zedstore.ThreadMessageKindCompaction:
		if !opts.IncludeSystemMessages || message.Compaction == nil {
			return emptyMessage(), false
		}

		compaction, text := compactionMetadata(message.Compaction)
		if compaction == nil || strings.TrimSpace(text) == "" {
			return emptyMessage(), false
		}

		return transcript.Message{
			UUID:              "",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "system",
			Visibility:        transcript.MessageVisibilityTranscriptOnly,
			Compaction:        compaction,
			Timestamp:         thread.UpdatedAt,
			Text:              text,
			Thinking:          "",
			HasTools:          false,
			Tools:             nil,
			Attachments:       nil,
			TextParts:         nil,
		}, true
	default:
		return emptyMessage(), false
	}
}

func userMessageText(message *zedstore.UserMessage) string {
	parts := make([]string, 0, len(message.Content))
	for _, part := range message.Content {
		switch part.Kind {
		case zedstore.UserContentKindText:
			if strings.TrimSpace(part.Text) != "" {
				parts = append(parts, part.Text)
			}
		case zedstore.UserContentKindMention:
			if part.Mention == nil {
				continue
			}

			if strings.TrimSpace(part.Mention.Content) != "" {
				parts = append(parts, part.Mention.Content)
			} else if strings.TrimSpace(part.Mention.URI) != "" {
				parts = append(parts, part.Mention.URI)
			}
		}
	}

	return strings.Join(parts, "\n")
}

func agentMessageParts(
	message *zedstore.AgentMessage,
	includeToolOutputs bool,
) (string, string, []transcript.ToolCall) {
	textParts := make([]string, 0, len(message.Content))
	thinkingParts := make([]string, 0, len(message.Content))
	tools := make([]transcript.ToolCall, 0)

	for _, part := range message.Content {
		switch part.Kind {
		case zedstore.AgentContentKindText:
			if strings.TrimSpace(part.Text) != "" {
				textParts = append(textParts, part.Text)
			}
		case zedstore.AgentContentKindThinking, zedstore.AgentContentKindRedactedThinking:
			if strings.TrimSpace(part.Text) != "" {
				thinkingParts = append(thinkingParts, part.Text)
			}
		case zedstore.AgentContentKindToolUse:
			if part.ToolUse == nil {
				continue
			}

			output, isError := "", false
			if includeToolOutputs {
				output, isError = zedToolResultOutput(message.ToolResults[part.ToolUse.ID])
			}

			input := transcript.ToolInputJSON{Raw: append([]byte(nil), part.ToolUse.Input...)}
			display, displayLang := toolDisplayText(part.ToolUse.Name, input)
			tools = append(
				tools,
				transcript.ToolCall{
					ID:               part.ToolUse.ID,
					Name:             part.ToolUse.Name,
					Input:            input,
					Display:          display,
					DisplayLang:      displayLang,
					Output:           output,
					IsError:          isError,
					Attachments:      nil,
					Timestamp:        nil,
					TimestampUnknown: false,
				},
			)
		}
	}

	return strings.Join(textParts, "\n"), strings.Join(thinkingParts, "\n"), tools
}
