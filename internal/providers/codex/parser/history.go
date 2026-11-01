package parser

import (
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

func codexTranscriptMessage(
	msg codexstore.HistoryMessage,
	includeSystemMessages bool,
) (transcript.Message, bool) {
	defer transcript.ContainMappingPanic()
	if msg.Role == "system" {
		if !includeSystemMessages {
			return emptyMessage(), false
		}
		return transcript.Message{
			UUID:              "",
			ParentUUID:        msg.ParentUUID,
			LogicalParentUUID: msg.LogicalParentUUID,
			Role:              msg.Role,
			Visibility:        msg.Visibility,
			Compaction:        msg.Compaction,
			Timestamp:         msg.Timestamp,
			Text:              msg.Text,
			Thinking:          "",
			HasTools:          false,
			Tools:             nil,
			Attachments:       nil,
			TextParts:         nil,
		}, true
	}

	text := transcript.NormalizeConversationOnlyText(msg.Text)
	if text == "" && msg.Thinking == "" && len(msg.Tools) == 0 {
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID:              "",
		ParentUUID:        msg.ParentUUID,
		LogicalParentUUID: msg.LogicalParentUUID,
		Role:              msg.Role,
		Visibility:        msg.Visibility,
		Compaction:        msg.Compaction,
		Timestamp:         msg.Timestamp,
		Text:              text,
		Thinking:          msg.Thinking,
		HasTools:          msg.HasTools,
		Tools:             msg.Tools,
		Attachments:       nil,
		TextParts:         codexTextParts(msg.TextParts),
	}, true
}

// codexTextParts applies NormalizeConversationOnlyText to ordinary parts,
// preserves injected transcript text and its spans, and omits parts with empty
// text.
func codexTextParts(parts []transcript.MessageTextPart) []transcript.MessageTextPart {
	if len(parts) == 0 {
		return nil
	}
	normalized := make([]transcript.MessageTextPart, 0, len(parts))
	for _, part := range parts {
		text := part.Text
		if !part.InjectedTranscript {
			text = transcript.NormalizeConversationOnlyText(text)
		}
		if text == "" {
			continue
		}
		normalized = append(normalized, transcript.MessageTextPart{
			Text:               text,
			InjectedTranscript: part.InjectedTranscript,
			TranscriptSpans:    part.TranscriptSpans,
		})
	}
	return normalized
}
