package claude

import (
	"os"
	"time"

	itranscript "goodkind.io/clyde/internal/transcript"
)

// RecentMessage holds a single user or assistant message extracted from a transcript.
type RecentMessage struct {
	Role      string    // "user" or "assistant"
	Text      string    // truncated content
	Timestamp time.Time // zero if absent or unparseable
}

// ToolUseCount is a <tool, count> pair used for transcript analytics.
type ToolUseCount struct {
	Name  string
	Count int
}

// TranscriptStats contains cheap transcript-level counters for the TUI details
// pane. Token counts are Claude-style rough estimates, not count_tokens API
// values, so they are safe to compute on every selected transcript.
type TranscriptStats struct {
	VisibleMessages       int
	VisibleTokensEstimate int
	LastMessageTokens     int
	CompactionCount       int
	LastPreCompactTokens  int
}

// ExtractRecentMessages reads the tail of a transcript and returns the last n
// user/assistant messages with their text content truncated to maxLen chars.
func ExtractRecentMessages(transcriptPath string, n, maxLen int) []RecentMessage {
	all := LoadAllMessages(transcriptPath, maxLen)
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// LoadAllMessages reads the full transcript and returns every non-empty
// user/assistant message in order, each truncated to maxLen runes. Intended
// for the details pane when a session is selected. Large transcripts may
// produce a few thousand entries; at ~100 bytes each, that fits comfortably
// in memory without pagination.
func LoadAllMessages(transcriptPath string, maxLen int) []RecentMessage {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	parsed, err := itranscript.Parse(f)
	if err != nil {
		return nil
	}
	turns := itranscript.ShapeConversation(parsed, itranscript.ShapeOptions{
		ToolOnly:     itranscript.ToolOnlyCompactSummary,
		MaxTextRunes: maxLen,
	})
	out := make([]RecentMessage, 0, len(turns))
	for _, turn := range turns {
		out = append(out, RecentMessage{
			Role:      turn.Role,
			Text:      turn.Text,
			Timestamp: turn.Timestamp,
		})
	}
	return out
}
