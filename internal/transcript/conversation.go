package transcript

import (
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// ToolOnlyMode controls how ShapeConversation renders turns that contain only
// tool calls.
type ToolOnlyMode string

const (
	// ToolOnlyOmit drops tool-only turns.
	ToolOnlyOmit ToolOnlyMode = "omit"
	// ToolOnlyCompactSummary renders one compact line from the tool names.
	ToolOnlyCompactSummary ToolOnlyMode = "compact_summary"
	// ToolOnlyInputSummary renders tool names and input descriptions when present.
	ToolOnlyInputSummary ToolOnlyMode = "input_summary"
	// ToolOnlyFullDetail renders tool input JSON and any loaded output text.
	ToolOnlyFullDetail ToolOnlyMode = "full_detail"
)

// ShapeOptions is part of Clyde's typed adapter surface.
type ShapeOptions struct {
	IncludeThinking  bool
	ConversationOnly bool
	ToolOnly         ToolOnlyMode
	MaxTextRunes     int
}

var (
	conversationOnlyExactDrops = map[string]bool{
		"No response requested.":                     true,
		"[Request interrupted by user]":              true,
		"[Request interrupted by user for tool use]": true,
	}
	conversationOnlyImageLineRe = regexp.MustCompile(`^\[Image(?::| #).*\]$`)
)

// ConversationTurn is part of Clyde's typed adapter surface.
type ConversationTurn struct {
	UUID       string    `json:"uuid,omitempty"`
	Role       string    `json:"role"`
	Timestamp  time.Time `json:"timestamp,omitzero"`
	Text       string    `json:"text"`
	Thinking   string    `json:"thinking,omitempty"`
	ToolNames  []string  `json:"tool_names,omitempty"`
	HasTools   bool      `json:"has_tools,omitempty"`
	IsToolOnly bool      `json:"is_tool_only,omitempty"`
}

// DefaultShapeOptions is part of Clyde's typed adapter surface.
func DefaultShapeOptions() ShapeOptions {
	return ShapeOptions{
		ToolOnly: ToolOnlyCompactSummary, IncludeThinking:

		// ShapeConversation is part of Clyde's typed adapter surface.
		false, ConversationOnly: false, MaxTextRunes: 0,
	}
}

// ShapeConversation is part of Clyde's typed adapter surface.
func ShapeConversation(messages []Message, opts ShapeOptions) []ConversationTurn {
	if opts.ToolOnly == "" {
		opts.ToolOnly = ToolOnlyCompactSummary
	}
	out := make([]ConversationTurn, 0, len(messages))
	for _, msg := range messages {
		turn := ConversationTurn{
			UUID:      msg.UUID,
			Role:      msg.Role,
			Timestamp: msg.Timestamp,
			HasTools:  msg.HasTools, Text: "", Thinking: "", ToolNames: nil, IsToolOnly: false,
		}
		text := normalizeConversationText(msg.Text, opts.MaxTextRunes, opts.ConversationOnly)
		thinking := ""
		if opts.IncludeThinking {
			thinking = normalizeConversationText(msg.Thinking, opts.MaxTextRunes, false)
		}
		if len(msg.Tools) > 0 {
			turn.ToolNames = msg.ToolNames()
		}
		if text == "" && msg.HasTools {
			turn.IsToolOnly = true
			if opts.ConversationOnly {
				continue
			}
			switch opts.ToolOnly {
			case ToolOnlyOmit:
				continue
			case ToolOnlyCompactSummary:
				text = toolSummaryText(turn.ToolNames)
			case ToolOnlyInputSummary:
				text = toolInputSummaryText(msg.Tools)
			case ToolOnlyFullDetail:
				text = toolFullDetailText(msg.Tools)
			default:
				text = toolSummaryText(turn.ToolNames)
			}
		}
		if text == "" && thinking == "" {
			continue
		}
		turn.Text = text
		turn.Thinking = thinking
		out = append(out, turn)
	}
	return out
}

// NormalizeConversationOnlyText applies the same text normalization the
// conversation-only shaping path uses, without reshaping message roles or tool
// structure.
func NormalizeConversationOnlyText(text string) string {
	return normalizeConversationText(text, 0, true)
}

func normalizeConversationText(text string, maxRunes int, conversationOnly bool) string {
	text = strings.ReplaceAll(text, "\r", "")
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	lastBlank := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if conversationOnly && shouldDropConversationOnlyLine(line) {
			continue
		}
		if line == "" {
			if lastBlank {
				continue
			}
			out = append(out, "")
			lastBlank = true
			continue
		}
		out = append(out, line)
		lastBlank = false
	}
	text = strings.TrimSpace(strings.Join(out, "\n"))
	if text == "" {
		return ""
	}
	if maxRunes > 0 {
		runes := []rune(text)
		if len(runes) > maxRunes {
			text = string(runes[:maxRunes]) + "..."
		}
	}
	return text
}

func shouldDropConversationOnlyLine(line string) bool {
	if line == "" {
		return false
	}
	if _, ok := conversationOnlyExactDrops[line]; ok {
		return true
	}
	return conversationOnlyImageLineRe.MatchString(line)
}

func toolSummaryText(names []string) string {
	if len(names) == 0 {
		return "[used tools]"
	}
	return "[used: " + strings.Join(names, ", ") + "]"
}

type toolDescriptionInput struct {
	Description string `json:"description"`
}

func toolInputSummaryText(tools []ToolCall) string {
	if len(tools) == 0 {
		return "[used tools]"
	}
	lines := make([]string, 0, len(tools))
	for _, tool := range tools {
		line := "[tool: " + tool.Name + "]"
		if description := toolInputDescription(tool.Input); description != "" {
			line += " " + description
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func toolInputDescription(input ToolInputJSON) string {
	if input.Len() == 0 {
		return ""
	}
	var parsed toolDescriptionInput
	if err := json.Unmarshal(input.Raw, &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Description)
}

func toolFullDetailText(tools []ToolCall) string {
	if len(tools) == 0 {
		return "[used tools]"
	}
	lines := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Input.Len() == 0 {
			lines = append(lines, "[tool: "+tool.Name+"]")
			continue
		}
		body, err := json.Marshal(&tool.Input)
		if err != nil {
			lines = append(lines, "[tool: "+tool.Name+"]")
			continue
		}
		line := fmt.Sprintf("[tool: %s] %s", tool.Name, string(body))
		if strings.TrimSpace(tool.Output) != "" {
			line += "\n[tool output]\n" + strings.TrimSpace(tool.Output)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// RenderMarkdownConversation is part of Clyde's typed adapter surface.
func RenderMarkdownConversation(turns []ConversationTurn) string {
	var b strings.Builder
	for _, turn := range turns {
		role := "User"
		if turn.Role == "assistant" {
			role = "Assistant"
		}
		if !turn.Timestamp.IsZero() {
			fmt.Fprintf(&b, "### %s (%s)\n\n", role, turn.Timestamp.Format("2006-01-02 15:04"))
		} else {
			fmt.Fprintf(&b, "### %s\n\n", role)
		}
		if turn.Text != "" {
			b.WriteString(turn.Text)
			b.WriteString("\n\n")
		}
		if turn.Thinking != "" {
			b.WriteString("> Thinking\n>\n")
			for line := range strings.SplitSeq(turn.Thinking, "\n") {
				b.WriteString("> " + line + "\n")
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// RenderHTMLConversation is part of Clyde's typed adapter surface.
func RenderHTMLConversation(turns []ConversationTurn) string {
	var b strings.Builder
	b.WriteString("<div class=\"conversation\">\n")
	for _, turn := range turns {
		role := "User"
		if turn.Role == "assistant" {
			role = "Assistant"
		}
		b.WriteString("<section class=\"turn\">\n")
		b.WriteString("<header><strong>" + html.EscapeString(role) + "</strong>")
		if !turn.Timestamp.IsZero() {
			b.WriteString(" <time>" + html.EscapeString(turn.Timestamp.Format("2006-01-02 15:04")) + "</time>")
		}
		b.WriteString("</header>\n")
		if turn.Text != "" {
			b.WriteString("<p>" + html.EscapeString(turn.Text) + "</p>\n")
		}
		if turn.Thinking != "" {
			b.WriteString("<blockquote><strong>Thinking</strong><br>" + strings.ReplaceAll(html.EscapeString(turn.Thinking), "\n", "<br>") + "</blockquote>\n")
		}
		b.WriteString("</section>\n")
	}
	b.WriteString("</div>")
	return b.String()
}

// RenderJSONConversation is part of Clyde's typed adapter surface.
func RenderJSONConversation(turns []ConversationTurn) ([]byte, error) {
	out, err := json.MarshalIndent(turns, "", "  ")
	if err != nil {
		slog.Warn("transcript.conversation.marshal_failed", "concern", "transcript", "err", err)
		return nil, fmt.Errorf("marshal conversation JSON: %w", err)
	}
	return out, nil
}
