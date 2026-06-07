package parser

import (
	"fmt"
	"log/slog"
	"time"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/clyde/internal/transcript"
)

// codexHistory is the provider-owned intermediate representation for a Codex
// rollout thread. It sits above raw rollout discovery and below generic
// conversation shaping, owning the one call into the codex store.
type codexHistory struct {
	modelProvider string
	messages      []codexHistoryMessage
}

type codexHistoryMessage struct {
	Role      string
	Text      string
	Timestamp time.Time
}

// readCodexHistory reads a Codex rollout thread with its full message history
// and exposes it through the shared transcript shaping layer.
func readCodexHistory(path string) (codexHistory, error) {
	thread, err := codexstore.ReadThreadByRolloutPath(path, true, false)
	if err != nil {
		slog.Warn("providers.codex.parser.read_failed",
			"component", "codex",
			"subcomponent", "parser",
			"concern", slogger.ConcernProviderCodexStore,
			"path", path,
			"err", err,
		)
		return codexHistory{}, fmt.Errorf("read codex rollout history: %w", err)
	}
	return codexHistoryFromThread(thread), nil
}

func codexHistoryFromThread(thread codexstore.ThreadSummary) codexHistory {
	messages := make([]codexHistoryMessage, 0, len(thread.Messages))
	for _, msg := range thread.Messages {
		messages = append(messages, codexHistoryMessage{
			Role:      msg.Role,
			Text:      msg.Text,
			Timestamp: msg.Timestamp,
		})
	}
	return codexHistory{
		modelProvider: thread.ModelProvider,
		messages:      messages,
	}
}

// conversationMessages shapes the Codex history into generic transcript
// messages, applying the same conversation-only normalization the prior loader
// used (ConversationOnly with tool turns omitted and no rune cap) so the
// rendered, searched, and exported output is unchanged. The raw rollout
// messages carry no thinking or tool blocks, so shaping reduces to normalizing
// and dropping empty text turns.
func (h codexHistory) conversationMessages() []transcript.Message {
	shapeOpts := transcript.ShapeOptions{
		IncludeThinking:  false,
		ConversationOnly: true,
		MaxTextRunes:     0,
		ToolOnly:         transcript.ToolOnlyOmit,
	}
	raw := make([]transcript.Message, 0, len(h.messages))
	for _, msg := range h.messages {
		raw = append(raw, transcript.Message{
			UUID:      "",
			Role:      msg.Role,
			Timestamp: msg.Timestamp,
			Text:      msg.Text,
			Thinking:  "",
			HasTools:  false,
			Tools:     nil,
		})
	}
	turns := transcript.ShapeConversation(raw, shapeOpts)
	out := make([]transcript.Message, 0, len(turns))
	for _, turn := range turns {
		out = append(out, transcript.Message{
			UUID:      "",
			Role:      turn.Role,
			Timestamp: turn.Timestamp,
			Text:      turn.Text,
			Thinking:  "",
			HasTools:  false,
			Tools:     nil,
		})
	}
	return out
}
