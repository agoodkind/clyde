package parser

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

// parseOptions controls noise stripping while parsing Claude transcript lines.
type parseOptions struct {
	PreserveSystemPrompts bool
	IncludeSystemMessages bool
}

// contentBlockType enumerates the content-block "type" strings the Claude
// transcript parser walks on user and assistant entries.
type contentBlockType string

const (
	contentBlockText       contentBlockType = "text"
	contentBlockThinking   contentBlockType = "thinking"
	contentBlockToolUse    contentBlockType = "tool_use"
	contentBlockToolResult contentBlockType = "tool_result"
)

type claudeSystemSubtype string

const (
	claudeSystemSubtypeCompactBoundary      claudeSystemSubtype = "compact_boundary"
	claudeSystemSubtypeMicrocompactBoundary claudeSystemSubtype = "microcompact_boundary"
)

type claudeCompactionTriggerValue string

const (
	claudeCompactionTriggerManual claudeCompactionTriggerValue = "manual"
	claudeCompactionTriggerAuto   claudeCompactionTriggerValue = "auto"
)

// rawEntry mirrors one Claude transcript JSONL line.
type rawEntry struct {
	UUID                      string                    `json:"uuid"`
	ParentUUID                string                    `json:"parentUuid"`
	LogicalParentUUID         string                    `json:"logicalParentUuid"`
	Type                      string                    `json:"type"`
	Subtype                   claudeSystemSubtype       `json:"subtype"`
	Content                   string                    `json:"content"`
	IsMeta                    bool                      `json:"isMeta"`
	IsVisibleInTranscriptOnly bool                      `json:"isVisibleInTranscriptOnly"`
	IsCompactSummary          bool                      `json:"isCompactSummary"`
	CompactMetadata           *rawClaudeCompactMetadata `json:"compactMetadata"`
	Timestamp                 time.Time                 `json:"timestamp"`
	Message                   json.RawMessage           `json:"message"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type     string                   `json:"type"`
	Text     string                   `json:"text"`
	Thinking string                   `json:"thinking"`
	ID       string                   `json:"id"`
	Name     string                   `json:"name"`
	Input    transcript.ToolInputJSON `json:"input"`
}

type rawClaudeCompactMetadata struct {
	Trigger                 string                    `json:"trigger"`
	PreTokens               int                       `json:"preTokens"`
	PostTokens              int                       `json:"postTokens"`
	TokensSaved             int                       `json:"tokensSaved"`
	MessagesSummarized      int                       `json:"messagesSummarized"`
	ReplacementHistoryCount int                       `json:"replacementHistoryCount"`
	PreservedSegment        rawClaudePreservedSegment `json:"preservedSegment"`
}

type rawClaudePreservedSegment struct {
	HeadUUID   string `json:"headUuid"`
	AnchorUUID string `json:"anchorUuid"`
	TailUUID   string `json:"tailUuid"`
}

var (
	systemTagRe = regexp.MustCompile(`<(?:system-reminder|local-command[^>]*|command-name|command-message|command-args|local-command-stdout|local-command-caveat)[^>]*>[\s\S]*?</(?:system-reminder|local-command[^>]*|command-name|command-message|command-args|local-command-stdout|local-command-caveat)>`)
	noiseTags   = []string{
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

var noisePatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(noiseTags))
	for _, t := range noiseTags {
		out = append(out, regexp.MustCompile(`(?is)<`+t+`\b[^>]*>.*?</`+t+`>`))
	}
	return out
}()

// emptyMessage returns the fully zeroed message used for skipped or failed
// lines, written out so exhaustruct sees every field set.
func emptyMessage() transcript.Message {
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "",
		Visibility:        "",
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              "",
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}
}

// parseLine decodes one transcript line into a message. The boolean is false
// when the line is not a usable user, assistant, or opted-in system compaction
// turn.
func parseLine(line []byte, opts parseOptions) (transcript.Message, bool) {
	var entry rawEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return emptyMessage(), false
	}
	if entry.Type == "system" {
		if !opts.IncludeSystemMessages {
			return emptyMessage(), false
		}
		return parseSystemEntry(entry)
	}
	if entry.Type != "user" && entry.Type != "assistant" {
		return emptyMessage(), false
	}
	if len(entry.Message) == 0 {
		return emptyMessage(), false
	}

	var msg rawMessage
	if err := json.Unmarshal(entry.Message, &msg); err != nil {
		return emptyMessage(), false
	}

	m := transcript.Message{
		UUID:              entry.UUID,
		ParentUUID:        entry.ParentUUID,
		LogicalParentUUID: entry.LogicalParentUUID,
		Role:              entry.Type,
		Visibility:        messageVisibility(entry.IsMeta, entry.IsVisibleInTranscriptOnly),
		Compaction:        nil,
		Timestamp:         entry.Timestamp,
		Text:              "",
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}

	if entry.Type == "user" {
		text := extractUserText(msg.Content)
		if text == "" {
			// tool result entry, skip
			return emptyMessage(), false
		}
		if !opts.PreserveSystemPrompts {
			text = stripSystemTags(text)
		}
		m.Text = strings.TrimSpace(text)
		if entry.IsCompactSummary {
			m.Compaction = claudeCompactionMetadata(
				transcript.CompactionKindSummary,
				entry.CompactMetadata,
			)
		}
		return m, m.Text != ""
	}

	// Assistant: content is an array of blocks.
	parseAssistantBlocks(&m, msg.Content)
	// Include assistant messages even if Text is empty (may have only tool calls).
	return m, m.Text != "" || m.HasTools
}

func parseSystemEntry(entry rawEntry) (transcript.Message, bool) {
	var kind transcript.CompactionKind
	switch entry.Subtype {
	case claudeSystemSubtypeCompactBoundary:
		kind = transcript.CompactionKindBoundary
	case claudeSystemSubtypeMicrocompactBoundary:
		kind = transcript.CompactionKindMicroboundary
	default:
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID:              entry.UUID,
		ParentUUID:        entry.ParentUUID,
		LogicalParentUUID: entry.LogicalParentUUID,
		Role:              "system",
		Visibility:        messageVisibility(entry.IsMeta, entry.IsVisibleInTranscriptOnly),
		Compaction:        claudeCompactionMetadata(kind, entry.CompactMetadata),
		Timestamp:         entry.Timestamp,
		Text:              strings.TrimSpace(entry.Content),
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}, true
}

func messageVisibility(isMeta bool, transcriptOnly bool) transcript.MessageVisibility {
	if isMeta {
		return transcript.MessageVisibilityMetaOnly
	}
	if transcriptOnly {
		return transcript.MessageVisibilityTranscriptOnly
	}
	return transcript.MessageVisibilityVisible
}

func claudeCompactionMetadata(
	kind transcript.CompactionKind,
	raw *rawClaudeCompactMetadata,
) *transcript.CompactionMetadata {
	metadata := &transcript.CompactionMetadata{
		Kind:                    kind,
		Trigger:                 transcript.CompactionTriggerUnknown,
		PreTokens:               0,
		PostTokens:              0,
		TokensSaved:             0,
		MessagesSummarized:      0,
		ReplacementHistoryCount: 0,
		HeadUUID:                "",
		AnchorUUID:              "",
		TailUUID:                "",
	}
	if raw == nil {
		return metadata
	}
	metadata.Trigger = claudeCompactionTrigger(raw.Trigger)
	metadata.PreTokens = raw.PreTokens
	metadata.PostTokens = raw.PostTokens
	metadata.TokensSaved = raw.TokensSaved
	if metadata.TokensSaved == 0 && raw.PreTokens > 0 && raw.PostTokens > 0 && raw.PreTokens >= raw.PostTokens {
		metadata.TokensSaved = raw.PreTokens - raw.PostTokens
	}
	metadata.MessagesSummarized = raw.MessagesSummarized
	metadata.ReplacementHistoryCount = raw.ReplacementHistoryCount
	metadata.HeadUUID = raw.PreservedSegment.HeadUUID
	metadata.AnchorUUID = raw.PreservedSegment.AnchorUUID
	metadata.TailUUID = raw.PreservedSegment.TailUUID
	return metadata
}

func claudeCompactionTrigger(value string) transcript.CompactionTrigger {
	switch claudeCompactionTriggerValue(strings.TrimSpace(strings.ToLower(value))) {
	case claudeCompactionTriggerManual:
		return transcript.CompactionTriggerManual
	case claudeCompactionTriggerAuto:
		return transcript.CompactionTriggerAuto
	default:
		return transcript.CompactionTriggerUnknown
	}
}

// extractUserText gets the text from a user message's content field.
// User messages have content as a string (older format) or an array of blocks (newer format).
// Array content may contain text blocks (user-authored) or tool_result blocks (skip those).
func extractUserText(raw json.RawMessage) string {
	// Try string content first (older Claude Code format).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	// Try array content: extract text blocks, ignore tool_result blocks.
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	hasText := false
	var parts []string
	for _, b := range blocks {
		switch contentBlockType(b.Type) {
		case contentBlockText:
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
				hasText = true
			}
		case contentBlockToolResult:
			// tool results are not user-authored text, skip
		case contentBlockThinking, contentBlockToolUse:
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
func parseAssistantBlocks(m *transcript.Message, raw json.RawMessage) {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return
	}

	var textParts []string
	for _, b := range blocks {
		switch contentBlockType(b.Type) {
		case contentBlockText:
			if t := strings.TrimSpace(b.Text); t != "" {
				textParts = append(textParts, t)
			}
		case contentBlockThinking:
			if t := strings.TrimSpace(b.Thinking); t != "" {
				m.Thinking = t
			}
		case contentBlockToolUse:
			m.HasTools = true
			m.Tools = append(m.Tools, transcript.ToolCall{
				ID:      b.ID,
				Name:    b.Name,
				Input:   b.Input,
				Output:  "",
				IsError: false,
			})
		case contentBlockToolResult:
			// Assistant entries should not carry tool results; the
			// assistant aggregator ignores them if they appear.
		}
	}
	m.Text = strings.Join(textParts, "\n\n")
}

// stripSystemTags removes system-injected tags from user messages.
func stripSystemTags(s string) string {
	s = systemTagRe.ReplaceAllString(s, "")
	for _, re := range noisePatterns {
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
