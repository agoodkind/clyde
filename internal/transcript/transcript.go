package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// transcriptContentBlockType enumerates the content-block "type"
// strings the transcript parser walks on user and assistant entries.
type transcriptContentBlockType string

const (
	transcriptContentBlockText       transcriptContentBlockType = "text"
	transcriptContentBlockThinking   transcriptContentBlockType = "thinking"
	transcriptContentBlockToolUse    transcriptContentBlockType = "tool_use"
	transcriptContentBlockToolResult transcriptContentBlockType = "tool_result"
)

// Message represents a single parsed conversation turn.
type Message struct {
	UUID      string     // entry UUID (for linking to tool results)
	Role      string     // "user" or "assistant"
	Timestamp time.Time  // when this entry was created
	Text      string     // concatenated text blocks (no tool calls, no thinking)
	Thinking  string     // thinking block text (for HTML export)
	HasTools  bool       // true if assistant message contained tool_use blocks
	Tools     []ToolCall // parsed tool calls with inputs
}

// ToolInputJSON keeps the transcript's tool_use.input payload opaque at the
// parse boundary. Claude tool inputs vary by tool and mirror the upstream
// transcript content-block shape documented in research/claude-code-source-code-full/src/services/api/claude.ts.
// The transcript package only stores and re-renders this JSON; business logic
// should decode a concrete schema at a narrower call site when it needs one.
type ToolInputJSON struct {
	Raw json.RawMessage
}

// UnmarshalJSON is part of Clyde's typed adapter surface.
func (j *ToolInputJSON) UnmarshalJSON(data []byte) error {
	if j == nil {
		return nil
	}
	j.Raw = append(j.Raw[:0], data...)
	return nil
}

// MarshalJSON is part of Clyde's typed adapter surface.
func (j *ToolInputJSON) MarshalJSON() ([]byte, error) {
	if j == nil {
		return []byte("null"), nil
	}
	if len(j.Raw) == 0 {
		return []byte("null"), nil
	}
	return j.Raw, nil
}

// Len is part of Clyde's typed adapter surface.
func (j *ToolInputJSON) Len() int {
	if j == nil {
		return 0
	}
	return len(j.Raw)
}

// ToolCall represents a single tool invocation within an assistant message.
type ToolCall struct {
	ID      string        // tool_use_id (links to tool_result in next user message)
	Name    string        // e.g. "Bash", "Edit", "Read"
	Input   ToolInputJSON // opaque tool input payload, preserved verbatim
	Output  string        // tool result text (loaded on demand, empty by default)
	IsError bool          // true if tool result was an error
}

// ParseOptions is part of Clyde's typed adapter surface.
type ParseOptions struct {
	PreserveSystemPrompts bool
}

// ParseOptions is part of Clyde's typed adapter surface.

// ToolNames returns the names of all tools used in this message.
func (m *Message) ToolNames() []string {
	names := make([]string, len(m.Tools))
	for i, t := range m.Tools {
		names[i] = t.Name
	}
	return names
}

// raw JSON structures for parsing transcript entries
type rawEntry struct {
	UUID      string          `json:"uuid"`
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type     string        `json:"type"`
	Text     string        `json:"text"`
	Thinking string        `json:"thinking"`
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Input    ToolInputJSON `json:"input"`
}

var (
	systemTagRe         = regexp.MustCompile(`<(?:system-reminder|local-command[^>]*|command-name|command-message|command-args|local-command-stdout|local-command-caveat)[^>]*>[\s\S]*?</(?:system-reminder|local-command[^>]*|command-name|command-message|command-args|local-command-stdout|local-command-caveat)>`)
	transcriptNoiseTags = []string{
		"command-name",
		"command-message",
		"command-args",
		"local-command-stdout",
		"local-command-stderr",
		"local-command-caveat",
		"system-reminder",
		"user-prompt-submit-hook",
		"task-notification",
		"bash-stdout",
		"bash-stderr",
	}
)

var transcriptNoisePatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(transcriptNoiseTags))
	for _, t := range transcriptNoiseTags {
		out = append(out, regexp.MustCompile(`(?is)<`+t+`\b[^>]*>.*?</`+t+`>`))
	}
	return out
}()

// ParseWithOptions is part of Clyde's typed adapter surface.
func ParseWithOptions(r io.Reader, opts ParseOptions) ([]Message, error) {
	reader := bufio.NewReader(r)

	// ParseWithOptions is part of Clyde's typed adapter surface.
	var messages []Message
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) > 0 {
				if msg, ok := parseLine(line, opts); ok {
					messages = append(messages, msg)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			slog.Warn("transcript.parse.read_line_failed", "concern", "transcript", "err", err)
			return messages, fmt.Errorf("read transcript line: %w", err)
		}
	}
	return messages, nil
}

func parseLine(line []byte, opts ParseOptions) (Message, bool) {
	var entry rawEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return Message{
			UUID: "", Role: "", Timestamp: time.
				Time{},

			Text: "", Thinking: "", HasTools: false, Tools: nil,
		}, false
	}
	if entry.Type != "user" && entry.Type != "assistant" {
		return Message{
			UUID: "", Role: "", Timestamp: time.
				Time{},

			Text: "", Thinking: "", HasTools: false, Tools: nil,
		}, false
	}
	if len(entry.Message) == 0 {
		return Message{
			UUID: "", Role: "", Timestamp: time.
				Time{},

			Text: "", Thinking: "", HasTools: false, Tools: nil,
		}, false
	}

	var msg rawMessage
	if err := json.Unmarshal(entry.Message, &msg); err != nil {
		return Message{
			UUID: "", Role: "", Timestamp: time.
				Time{},

			Text: "", Thinking: "", HasTools: false, Tools: nil,
		}, false
	}

	m := Message{
		UUID:      entry.UUID,
		Role:      entry.Type,
		Timestamp: entry.Timestamp, Text: "", Thinking: "", HasTools: false, Tools: nil,
	}

	if entry.Type == "user" {
		text := extractUserText(msg.Content)
		if text == "" {
			return Message{
				UUID: "", Role: // tool result entry, skip
				"", Timestamp: time.
					Time{},

				Text: "", Thinking: "", HasTools: false, Tools: nil,
			}, false
		}
		if !opts.PreserveSystemPrompts {
			text = stripSystemTags(text)
		}
		m.Text = strings.TrimSpace(text)
		return m, m.Text != ""
	}

	// Assistant: content is an array of blocks
	parseAssistantBlocks(&m, msg.Content)
	// Include assistant messages even if Text is empty (may have only tool calls)
	return m, m.Text != "" || m.HasTools
}

// extractUserText gets the text from a user message's content field.
// User messages have content as a string (older format) or an array of blocks (newer format).
// Array content may contain text blocks (user-authored) or tool_result blocks (skip those).
func extractUserText(raw json.RawMessage) string {
	// Try string content first (older Claude Code format)
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	// Try array content: extract text blocks, ignore tool_result blocks
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	hasText := false
	var parts []string
	for _, b := range blocks {
		switch transcriptContentBlockType(b.Type) {
		case transcriptContentBlockText:
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
				hasText = true
			}
		case transcriptContentBlockToolResult:
			// tool results are not user-authored text, skip
		case transcriptContentBlockThinking, transcriptContentBlockToolUse:
			// User entries do not normally carry these block kinds,
			// and the user-text aggregator ignores them when they
			// appear.
		}
	}
	if !hasText {
		return "" // only tool results, skip the entry
	}
	return strings.Join(parts, "\n")
}

// parseAssistantBlocks extracts text, thinking, and tool calls from an assistant message.
func parseAssistantBlocks(m *Message, raw json.RawMessage) {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return
	}

	var textParts []string
	for _, b := range blocks {
		switch transcriptContentBlockType(b.Type) {
		case transcriptContentBlockText:
			if t := strings.TrimSpace(b.Text); t != "" {
				textParts = append(textParts, t)
			}
		case transcriptContentBlockThinking:
			if t := strings.TrimSpace(b.Thinking); t != "" {
				m.Thinking = t
			}
		case transcriptContentBlockToolUse:
			m.HasTools = true
			m.Tools = append(m.Tools, ToolCall{
				ID:    b.ID,
				Name:  b.Name,
				Input: b.Input, Output: "", IsError: false,
			})
		case transcriptContentBlockToolResult:
			// Assistant entries should not carry tool results; the
			// assistant aggregator ignores them if they appear.
		}
	}
	m.Text = strings.Join(textParts, "\n\n")
}

// stripSystemTags removes system-injected tags from user messages.
func stripSystemTags(s string) string {
	s = systemTagRe.ReplaceAllString(s, "")
	for _, re := range transcriptNoisePatterns {
		s = re.ReplaceAllString(s, "")
	}
	if idx := strings.Index(s, "<"); idx == 0 {
		if end := strings.Index(s, ">"); end > 0 && end < 80 {
			s = s[end+1:]
		}
	}
	if strings.Contains(s, "hook feedback:") {
		var keep []string
		for line := range strings.SplitSeq(s, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "Stop hook feedback:") ||
				strings.HasPrefix(t, "PreToolUse hook feedback:") ||
				strings.HasPrefix(t, "PostToolUse hook feedback:") ||
				strings.HasPrefix(t, "UserPromptSubmit hook feedback:") {
				continue
			}
			keep = append(keep, line)
		}
		s = strings.Join(keep, "\n")
	}
	return strings.TrimSpace(s)
}
