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

// ContextWindowMaxTextRunes bounds one message in a context window.
//
// A context window previews what surrounds a point in a conversation, so it is
// sized for reading; an export carries no cap because it is the document. The
// bound matters now that one message is a whole turn: the largest turn in the
// observed Cursor corpus renders to roughly 300 KB, so an uncapped five-message
// window returns most of a megabyte where it used to return five short records.
const ContextWindowMaxTextRunes = 2000

// ContextWindowShapeOptions is the shaping a context window uses: the default
// rendering, bounded per message.
func ContextWindowShapeOptions() ShapeOptions {
	opts := DefaultShapeOptions()
	opts.MaxTextRunes = ContextWindowMaxTextRunes
	return opts
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
		// The cap bounds the prose, which is the part that grows without limit now
		// that one message is a whole turn. The tool block is appended after it and
		// is not truncated: capping the joined text cut the block off a turn whose
		// prose already filled the budget, which loses the record that the turn
		// called anything at all. That is information the model cannot recover,
		// where prose is merely shortened and marked.
		text := normalizeConversationText(msg.Text, opts.MaxTextRunes, opts.ConversationOnly)
		thinking := ""
		if opts.IncludeThinking {
			thinking = normalizeConversationText(msg.Thinking, opts.MaxTextRunes, false)
		}
		if len(msg.Tools) > 0 {
			turn.ToolNames = msg.ToolNames()
		}
		turn.IsToolOnly = msg.HasTools && text == ""
		text, keepTurn := textWithTools(text, turn.ToolNames, msg, opts)
		if !keepTurn {
			continue
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
	return capRunes(text, maxRunes)
}

// capRunes truncates text to maxRunes and marks it, leaving it unchanged when
// no cap is set.
func capRunes(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "..."
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

// textWithTools folds a turn's tool calls into its rendered text. A turn that
// holds prose and tool calls together keeps both, because gating the tool
// rendering on empty text hid every tool call in a turn that also said
// something. It reports false when the turn has nothing left to show.
func textWithTools(text string, names []string, msg Message, opts ShapeOptions) (string, bool) {
	if !msg.HasTools {
		return text, true
	}
	// The conversation-only view is prose, so it renders no tool lines and keeps
	// a turn only for what it said.
	if opts.ConversationOnly {
		return text, text != ""
	}
	toolText := renderToolText(opts.ToolOnly, names, msg.Tools)
	if toolText == "" {
		// The omit mode renders no tools, so a tool-only turn has nothing to show
		// while one that also carried prose keeps the prose.
		return text, text != ""
	}
	// A tool-only turn is entirely its calls, so their position is unambiguous.
	if text == "" {
		return toolText, true
	}
	return text + "\n" + labeledToolBlock(opts.ToolOnly, names, toolText), true
}

// mixedTurnToolHeading introduces the tool block on a turn that also carries
// prose. [Message] models a turn as text plus a separate tool list, with no way
// to record where in the text each call happened, so the block is labeled as the
// turn's calls in call order rather than written as though it followed the
// prose. Appending it unlabeled would assert an order the model cannot know.
const mixedTurnToolHeading = "[tool calls in this turn]"

// labeledToolBlock names a mixed turn's tool block so the rendering claims only
// what the model records: which calls the turn made, and their order relative to
// each other.
func labeledToolBlock(mode ToolOnlyMode, names []string, toolText string) string {
	if mode == ToolOnlyCompactSummary || mode == "" {
		if len(names) == 0 {
			return mixedTurnToolHeading
		}
		return "[tool calls in this turn: " + strings.Join(names, ", ") + "]"
	}
	return mixedTurnToolHeading + "\n" + toolText
}

// renderToolText renders a turn's tool calls at the selected level of detail.
// It returns the empty string for [ToolOnlyOmit], which is the caller's signal
// that this turn shows no tools at all.
func renderToolText(mode ToolOnlyMode, names []string, tools []ToolCall) string {
	switch mode {
	case ToolOnlyOmit:
		return ""
	case ToolOnlyCompactSummary:
		return toolSummaryText(names)
	case ToolOnlyInputSummary:
		return toolInputSummaryText(tools)
	case ToolOnlyFullDetail:
		return toolFullDetailText(tools)
	default:
		return toolSummaryText(names)
	}
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
		// The call's display text and its output are separate pieces of what the
		// tool did, so a call whose shape the parser did not recognize still
		// renders whatever it returned.
		line := "[tool: " + tool.Name + "]"
		if display := strings.TrimSpace(tool.Display); display != "" {
			line += " " + display
		}
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
