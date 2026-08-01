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
	}, true
}
