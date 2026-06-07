package parser

import (
	"fmt"
	"log/slog"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/clyde/internal/transcript"
)

// readCodexHistory reads a Codex rollout thread with its full message history
// and maps it directly onto shared transcript messages. It preserves the prior
// conversation-only text normalization for user and assistant turns so the
// default rendered and searched output is unchanged, while leaving compaction
// boundary metadata on the raw shared stream for opt-in callers.
func readCodexHistory(path string, includeSystemMessages bool) ([]transcript.Message, error) {
	thread, err := codexstore.ReadThreadByRolloutPath(path, true, false)
	if err != nil {
		slog.Warn("providers.codex.parser.read_failed",
			"component", "codex",
			"subcomponent", "parser",
			"concern", slogger.ConcernProviderCodexStore,
			"path", path,
			"err", err,
		)
		return nil, fmt.Errorf("read codex rollout history: %w", err)
	}
	return codexMessagesFromThread(thread, includeSystemMessages), nil
}

func codexMessagesFromThread(
	thread codexstore.ThreadSummary,
	includeSystemMessages bool,
) []transcript.Message {
	messages := make([]transcript.Message, 0, len(thread.Messages))
	for _, msg := range thread.Messages {
		message, ok := codexTranscriptMessage(msg, includeSystemMessages)
		if !ok {
			continue
		}
		messages = append(messages, message)
	}
	return messages
}

func codexTranscriptMessage(
	msg codexstore.HistoryMessage,
	includeSystemMessages bool,
) (transcript.Message, bool) {
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
	if text == "" {
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
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}, true
}
