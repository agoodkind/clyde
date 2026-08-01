package parser

import (
	"encoding/json"
	"log/slog"
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

type claudeCompactionTriggerValue string

const (
	claudeCompactionTriggerManual claudeCompactionTriggerValue = "manual"
	claudeCompactionTriggerAuto   claudeCompactionTriggerValue = "auto"
)

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentBlock is one block of a message's content array, for every block kind
// a user or assistant entry carries. One type covers all of them because the
// content array is decoded once per line: a second decoder over the same bytes
// would read the file twice and would not reach the decode report the entry
// tree keeps.
type contentBlock struct {
	Type     string                   `json:"type"`
	Text     string                   `json:"text"`
	Thinking string                   `json:"thinking"`
	ID       string                   `json:"id"`
	Name     string                   `json:"name"`
	Input    transcript.ToolInputJSON `json:"input"`

	// tool_result blocks, which a user entry carries for the tools the
	// assistant turn before it ran.
	ToolUseID string               `json:"tool_use_id"`
	Content   ToolUseResultContent `json:"content"`
	IsError   bool                 `json:"is_error"`
}

// claudeToolResult is what one tool returned, keyed by the call it answers.
type claudeToolResult struct {
	ToolUseID string
	Output    string
	IsError   bool
}

// toolResult reads what a person saw in this tool_result block. A content shape
// the parser could not read is logged and kept as it arrived, so it is visible
// rather than resolving silently to nothing.
func (block contentBlock) toolResult() claudeToolResult {
	output, decode := block.Content.SearchableText()
	if decode == FieldDecodePartial {
		slog.Warn("providers.claude.parser.tool_result_content_partial",
			"concern", concern, "component", "claude", "tool_use_id", block.ToolUseID)
	}
	return claudeToolResult{ToolUseID: block.ToolUseID, Output: output, IsError: block.IsError}
}

type rawClaudeBoundaryMetadata struct {
	Trigger                   string                    `json:"trigger"`
	PreTokens                 int                       `json:"preTokens"`
	PostTokens                int                       `json:"postTokens"`
	TokensSaved               int                       `json:"tokensSaved"`
	MessagesSummarized        int                       `json:"messagesSummarized"`
	ReplacementHistoryCount   int                       `json:"replacementHistoryCount"`
	UserContext               string                    `json:"userContext"`
	Direction                 string                    `json:"direction"`
	PreCompactDiscoveredTools []string                  `json:"preCompactDiscoveredTools"`
	PreservedSegment          rawClaudePreservedSegment `json:"preservedSegment"`
}

type rawClaudeMicrocompactMetadata struct {
	Trigger                string   `json:"trigger"`
	PreTokens              int      `json:"preTokens"`
	PostTokens             int      `json:"postTokens"`
	TokensSaved            int      `json:"tokensSaved"`
	MessagesSummarized     int      `json:"messagesSummarized"`
	UserContext            string   `json:"userContext"`
	Direction              string   `json:"direction"`
	CompactedToolIDs       []string `json:"compactedToolIds"`
	ClearedAttachmentUUIDs []string `json:"clearedAttachmentUUIDs"`
}

type rawClaudeSummarizeMetadata struct {
	MessagesSummarized int    `json:"messagesSummarized"`
	UserContext        string `json:"userContext"`
	Direction          string `json:"direction"`
}

func emptyRawClaudeBoundaryMetadata() rawClaudeBoundaryMetadata {
	return rawClaudeBoundaryMetadata{
		Trigger:                   "",
		PreTokens:                 0,
		PostTokens:                0,
		TokensSaved:               0,
		MessagesSummarized:        0,
		ReplacementHistoryCount:   0,
		UserContext:               "",
		Direction:                 "",
		PreCompactDiscoveredTools: nil,
		PreservedSegment: rawClaudePreservedSegment{
			HeadUUID:   "",
			AnchorUUID: "",
			TailUUID:   "",
		},
	}
}

func emptyRawClaudeMicrocompactMetadata() rawClaudeMicrocompactMetadata {
	return rawClaudeMicrocompactMetadata{
		Trigger:                "",
		PreTokens:              0,
		PostTokens:             0,
		TokensSaved:            0,
		MessagesSummarized:     0,
		UserContext:            "",
		Direction:              "",
		CompactedToolIDs:       nil,
		ClearedAttachmentUUIDs: nil,
	}
}

func emptyRawClaudeSummarizeMetadata() rawClaudeSummarizeMetadata {
	return rawClaudeSummarizeMetadata{
		MessagesSummarized: 0,
		UserContext:        "",
		Direction:          "",
	}
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

// parseLine decodes one transcript line into a message and whatever tool
// results it carried. The boolean is false when the line is not a usable user,
// assistant, or opted-in system compaction turn.
//
// The results are returned even when the boolean is false, because a user entry
// holding only tool results is not a turn while its results still answer the
// assistant turn before it.
func parseLine(line []byte, opts parseOptions) (transcript.Message, []claudeToolResult, bool) {
	defer transcript.ContainMappingPanic()
	entry, err := DecodeTranscriptEntry(line)
	if err != nil {
		return emptyMessage(), nil, false
	}
	if entry.Type == EntryTypeSystem {
		if !opts.IncludeSystemMessages {
			return emptyMessage(), nil, false
		}
		systemMessage, ok := parseSystemEntry(entry)
		return systemMessage, nil, ok
	}
	if entry.Type != EntryTypeUser && entry.Type != EntryTypeAssistant {
		return emptyMessage(), nil, false
	}
	if len(entry.Message) == 0 {
		return emptyMessage(), nil, false
	}

	var msg rawMessage
	if err := json.Unmarshal(entry.Message, &msg); err != nil {
		return emptyMessage(), nil, false
	}

	m := transcript.Message{
		UUID:              entry.UUID,
		ParentUUID:        entry.ParentUUID,
		LogicalParentUUID: entry.LogicalParentUUID,
		Role:              string(entry.Type),
		Visibility:        messageVisibility(entry.IsMeta, entry.IsVisibleInTranscriptOnly),
		Compaction:        nil,
		Timestamp:         entry.Timestamp.Time,
		Text:              "",
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}

	if entry.Type == EntryTypeUser {
		content := decodeUserContent(msg.Content)
		if content.Text == "" {
			// tool result entry, skip the turn and keep its results
			return emptyMessage(), content.Results, false
		}
		text := content.Text
		if !opts.PreserveSystemPrompts {
			text = stripSystemTags(text)
		}
		m.Text = strings.TrimSpace(text)
		if entry.IsCompactSummary {
			m.Compaction = claudeSummaryMetadata(
				entry.SummarizeMetadata,
				entry.Message,
				msg.Content,
				m.Text,
			)
		}
		return m, content.Results, m.Text != ""
	}

	// Assistant: content is an array of blocks.
	parseAssistantBlocks(&m, msg.Content)
	// Include assistant messages even if Text is empty: they may carry only
	// tool calls, or only a thinking block. Claude streams each content block as
	// its own assistant record, so a thinking block often lands in an entry with
	// no text and no tool_use; dropping it would lose the thinking from export.
	return m, nil, m.Text != "" || m.HasTools || m.Thinking != ""
}

func parseSystemEntry(entry TranscriptEntry) (transcript.Message, bool) {
	var metadata *transcript.CompactionMetadata
	switch entry.Subtype {
	case EntrySubtypeCompactBoundary:
		metadata = claudeBoundaryMetadata(entry.CompactMetadata)
	case EntrySubtypeMicrocompactBoundary:
		metadata = claudeMicrocompactMetadata(
			entry.MicrocompactMetadata,
			entry.CompactMetadata,
		)
	case EntrySubtypeUnspecified,
		EntrySubtypeTurnDuration,
		EntrySubtypeStopHookSummary,
		EntrySubtypeScheduledTaskFire,
		EntrySubtypeAwaySummary,
		EntrySubtypeLocalCommand,
		EntrySubtypeAPIError,
		EntrySubtypeBridgeStatus,
		EntrySubtypeModelRefusalNoFallback,
		EntrySubtypeModelRefusalFallback,
		EntrySubtypeInformational,
		EntrySubtypeAgentsKilled:
		// Session telemetry rather than a compaction boundary. These records
		// carry no conversation turn, so the transcript stream drops them.
		return emptyMessage(), false
	default:
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID:              entry.UUID,
		ParentUUID:        entry.ParentUUID,
		LogicalParentUUID: entry.LogicalParentUUID,
		Role:              "system",
		Visibility:        messageVisibility(entry.IsMeta, entry.IsVisibleInTranscriptOnly),
		Compaction:        metadata,
		Timestamp:         entry.Timestamp.Time,
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

func claudeBoundaryMetadata(raw json.RawMessage) *transcript.CompactionMetadata {
	metadata := &transcript.CompactionMetadata{
		Kind:                      transcript.CompactionKindBoundary,
		Trigger:                   transcript.CompactionTriggerUnknown,
		PreTokens:                 0,
		PostTokens:                0,
		TokensSaved:               0,
		MessagesSummarized:        0,
		ReplacementHistoryCount:   0,
		HeadUUID:                  "",
		AnchorUUID:                "",
		TailUUID:                  "",
		ContextItems:              nil,
		UserContext:               "",
		Direction:                 "",
		PreCompactDiscoveredTools: nil,
		CompactedToolIDs:          nil,
		ClearedAttachmentUUIDs:    nil,
		RawCompactMetadata:        cloneRawMessage(raw),
		RawMicrocompactMetadata:   nil,
		RawSummarizeMetadata:      nil,
	}
	parsed, ok := parseClaudeBoundaryMetadata(raw)
	if !ok {
		return metadata
	}
	metadata.Trigger = claudeCompactionTrigger(parsed.Trigger)
	metadata.PreTokens = parsed.PreTokens
	metadata.PostTokens = parsed.PostTokens
	metadata.TokensSaved = parsed.TokensSaved
	if metadata.TokensSaved == 0 &&
		parsed.PreTokens > 0 &&
		parsed.PostTokens > 0 &&
		parsed.PreTokens >= parsed.PostTokens {
		metadata.TokensSaved = parsed.PreTokens - parsed.PostTokens
	}
	metadata.MessagesSummarized = parsed.MessagesSummarized
	metadata.ReplacementHistoryCount = parsed.ReplacementHistoryCount
	metadata.HeadUUID = parsed.PreservedSegment.HeadUUID
	metadata.AnchorUUID = parsed.PreservedSegment.AnchorUUID
	metadata.TailUUID = parsed.PreservedSegment.TailUUID
	metadata.UserContext = parsed.UserContext
	metadata.Direction = parsed.Direction
	metadata.PreCompactDiscoveredTools = copyStringSlice(parsed.PreCompactDiscoveredTools)
	return metadata
}

func claudeMicrocompactMetadata(
	microcompactRaw json.RawMessage,
	legacyCompactRaw json.RawMessage,
) *transcript.CompactionMetadata {
	metadata := &transcript.CompactionMetadata{
		Kind:                      transcript.CompactionKindMicroboundary,
		Trigger:                   transcript.CompactionTriggerUnknown,
		PreTokens:                 0,
		PostTokens:                0,
		TokensSaved:               0,
		MessagesSummarized:        0,
		ReplacementHistoryCount:   0,
		HeadUUID:                  "",
		AnchorUUID:                "",
		TailUUID:                  "",
		ContextItems:              nil,
		UserContext:               "",
		Direction:                 "",
		PreCompactDiscoveredTools: nil,
		CompactedToolIDs:          nil,
		ClearedAttachmentUUIDs:    nil,
		RawCompactMetadata:        cloneRawMessage(legacyCompactRaw),
		RawMicrocompactMetadata:   cloneRawMessage(microcompactRaw),
		RawSummarizeMetadata:      nil,
	}
	parsed, ok := parseClaudeMicrocompactMetadata(microcompactRaw)
	if !ok {
		parsed, ok = parseClaudeMicrocompactMetadata(legacyCompactRaw)
		if !ok {
			return metadata
		}
	}
	metadata.Trigger = claudeCompactionTrigger(parsed.Trigger)
	metadata.PreTokens = parsed.PreTokens
	metadata.PostTokens = parsed.PostTokens
	metadata.TokensSaved = parsed.TokensSaved
	if metadata.TokensSaved == 0 &&
		parsed.PreTokens > 0 &&
		parsed.PostTokens > 0 &&
		parsed.PreTokens >= parsed.PostTokens {
		metadata.TokensSaved = parsed.PreTokens - parsed.PostTokens
	}
	metadata.MessagesSummarized = parsed.MessagesSummarized
	metadata.UserContext = parsed.UserContext
	metadata.Direction = parsed.Direction
	metadata.CompactedToolIDs = copyStringSlice(parsed.CompactedToolIDs)
	metadata.ClearedAttachmentUUIDs = copyStringSlice(
		parsed.ClearedAttachmentUUIDs,
	)
	return metadata
}

func claudeSummaryMetadata(
	summaryRaw json.RawMessage,
	messageRaw json.RawMessage,
	contentRaw json.RawMessage,
	text string,
) *transcript.CompactionMetadata {
	metadata := &transcript.CompactionMetadata{
		Kind:                      transcript.CompactionKindSummary,
		Trigger:                   transcript.CompactionTriggerUnknown,
		PreTokens:                 0,
		PostTokens:                0,
		TokensSaved:               0,
		MessagesSummarized:        0,
		ReplacementHistoryCount:   0,
		HeadUUID:                  "",
		AnchorUUID:                "",
		TailUUID:                  "",
		ContextItems:              []transcript.CompactedContextItem{claudeSummaryContextItem(messageRaw, contentRaw, text)},
		UserContext:               "",
		Direction:                 "",
		PreCompactDiscoveredTools: nil,
		CompactedToolIDs:          nil,
		ClearedAttachmentUUIDs:    nil,
		RawCompactMetadata:        nil,
		RawMicrocompactMetadata:   nil,
		RawSummarizeMetadata:      cloneRawMessage(summaryRaw),
	}
	parsed, ok := parseClaudeSummarizeMetadata(summaryRaw)
	if !ok {
		return metadata
	}
	metadata.MessagesSummarized = parsed.MessagesSummarized
	metadata.UserContext = parsed.UserContext
	metadata.Direction = parsed.Direction
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

func parseClaudeBoundaryMetadata(
	raw json.RawMessage,
) (rawClaudeBoundaryMetadata, bool) {
	if len(raw) == 0 {
		return emptyRawClaudeBoundaryMetadata(), false
	}
	var metadata rawClaudeBoundaryMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return emptyRawClaudeBoundaryMetadata(), false
	}
	return metadata, true
}

func parseClaudeMicrocompactMetadata(
	raw json.RawMessage,
) (rawClaudeMicrocompactMetadata, bool) {
	if len(raw) == 0 {
		return emptyRawClaudeMicrocompactMetadata(), false
	}
	var metadata rawClaudeMicrocompactMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return emptyRawClaudeMicrocompactMetadata(), false
	}
	return metadata, true
}

func parseClaudeSummarizeMetadata(
	raw json.RawMessage,
) (rawClaudeSummarizeMetadata, bool) {
	if len(raw) == 0 {
		return emptyRawClaudeSummarizeMetadata(), false
	}
	var metadata rawClaudeSummarizeMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return emptyRawClaudeSummarizeMetadata(), false
	}
	return metadata, true
}

func claudeSummaryContextItem(
	messageRaw json.RawMessage,
	contentRaw json.RawMessage,
	text string,
) transcript.CompactedContextItem {
	contentItem := transcript.CompactedMessageContentItem{
		Type:     "text",
		Text:     text,
		ImageURL: "",
		Detail:   "",
		Raw:      cloneRawMessage(contentRaw),
	}
	messageItem := transcript.CompactedMessageItem{
		Role:         "user",
		Phase:        "",
		Content:      []transcript.CompactedMessageContentItem{contentItem},
		ContentRaw:   cloneRawMessage(contentRaw),
		MessageClass: transcript.CompactedMessageClassSummary,
		Raw:          cloneRawMessage(messageRaw),
	}
	return transcript.CompactedContextItem{
		Kind:                 transcript.CompactedContextItemKindMessage,
		Message:              &messageItem,
		Reasoning:            nil,
		LocalShellCall:       nil,
		FunctionCall:         nil,
		ToolSearchCall:       nil,
		FunctionCallOutput:   nil,
		CustomToolCall:       nil,
		CustomToolCallOutput: nil,
		ToolSearchOutput:     nil,
		WebSearchCall:        nil,
		ImageGenerationCall:  nil,
		Compaction:           nil,
		CompactionTrigger:    nil,
		ContextCompaction:    nil,
		Other:                nil,
	}
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func copyStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

// userContent is what one user entry's content field holds: the text the person
// wrote, and what the tools the assistant ran before it returned.
//
// Both come out of one decode. An entry carrying only tool results has no text
// and is not a turn, yet its results belong to the assistant turn that ran
// those tools, so they are returned either way.
type userContent struct {
	Text    string
	Results []claudeToolResult
}

// decodeUserContent reads a user message's content field, which is a string in
// the older Claude Code format and an array of blocks in the newer one.
func decodeUserContent(raw json.RawMessage) userContent {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return userContent{Text: strings.TrimSpace(s), Results: nil}
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return userContent{Text: "", Results: nil}
	}
	content := userContent{Text: "", Results: nil}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch contentBlockType(b.Type) {
		case contentBlockText:
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		case contentBlockToolResult:
			content.Results = append(content.Results, b.toolResult())
		case contentBlockThinking, contentBlockToolUse:
			// User entries do not normally carry these block kinds,
			// and the user-text aggregator ignores them when they
			// appear.
		}
	}
	content.Text = strings.Join(parts, "\n")
	return content
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
			display, displayLang := toolDisplayText(b.Name, b.Input)
			m.Tools = append(m.Tools, transcript.ToolCall{
				ID:          b.ID,
				Name:        b.Name,
				Input:       b.Input,
				Display:     display,
				DisplayLang: displayLang,
				Output:      "",
				IsError:     false,
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
