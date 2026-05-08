package transcript

import (
	"fmt"
	"log/slog"
	"time"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
)

// CodexHistory is the shared, provider-owned IR for a Codex rollout thread.
// It stays above raw rollout discovery and below generic conversation shaping.
type CodexHistory struct {
	ModelProvider string
	messages      []codexHistoryMessage
}

type codexHistoryMessage struct {
	Role      string
	Text      string
	Timestamp time.Time
}

// ReadCodexHistory reads a Codex rollout thread and exposes it through the
// transcript package's shared shaping layer.
func ReadCodexHistory(path string) (CodexHistory, error) {
	thread, err := codexstore.ReadThreadByRolloutPath(path, true, false)
	if err != nil {
		slog.Warn("transcript.codex.read_failed",
			"path", path,
			"err", err,
		)
		return CodexHistory{}, fmt.Errorf("read codex rollout history: %w", err)
	}
	return codexHistoryFromThread(thread), nil
}

func codexHistoryFromThread(thread codexstore.ThreadSummary) CodexHistory {
	messages := make([]codexHistoryMessage, 0, len(thread.Messages))
	for _, msg := range thread.Messages {
		messages = append(messages, codexHistoryMessage{
			Role:      msg.Role,
			Text:      msg.Text,
			Timestamp: msg.Timestamp,
		})
	}
	return CodexHistory{
		ModelProvider: thread.ModelProvider,
		messages:      messages,
	}
}

// ConversationTurns shapes the Codex history into provider-neutral turns using
// the same normalization rules as Claude transcript shaping.
func (h CodexHistory) ConversationTurns(opts ShapeOptions) []ConversationTurn {
	turns := make([]ConversationTurn, 0, len(h.messages))
	for _, msg := range h.messages {
		text := normalizeConversationText(msg.Text, opts.MaxTextRunes, opts.ConversationOnly)
		if text == "" {
			continue
		}
		var turn ConversationTurn
		turn.Role = msg.Role
		turn.Timestamp = msg.Timestamp
		turn.Text = text
		turns = append(turns, turn)
	}
	return turns
}

// RecentConversationTurns returns the last shaped turns from the history.
func (h CodexHistory) RecentConversationTurns(limit int, opts ShapeOptions) []ConversationTurn {
	turns := h.ConversationTurns(opts)
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns
}
