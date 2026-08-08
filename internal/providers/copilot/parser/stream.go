package parser

import (
	"encoding/json"
	"iter"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

type userMessageData struct {
	Content     string            `json:"content"`
	Source      string            `json:"source"`
	Attachments []json.RawMessage `json:"attachments"`
}

type assistantMessageData struct {
	Content       string        `json:"content"`
	ReasoningText string        `json:"reasoningText"`
	ToolRequests  []toolRequest `json:"toolRequests"`
}

type toolRequest struct {
	ToolCallID       string          `json:"toolCallId"`
	Name             string          `json:"name"`
	Arguments        json.RawMessage `json:"arguments"`
	IntentionSummary string          `json:"intentionSummary"`
	ToolTitle        string          `json:"toolTitle"`
}

type toolCompletionData struct {
	ToolCallID string `json:"toolCallId"`
	Success    bool   `json:"success"`
	Result     struct {
		Content             string            `json:"content"`
		DetailedContent     string            `json:"detailedContent"`
		BinaryResultsForLLM []json.RawMessage `json:"binaryResultsForLlm"`
	} `json:"result"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

type compactionSourceTrigger string

const (
	compactionSourceTriggerManual    compactionSourceTrigger = "manual"
	compactionSourceTriggerThreshold compactionSourceTrigger = "threshold"
	compactionSourceTriggerAuto      compactionSourceTrigger = "auto"
)

// StreamSelected yields only the selected root or subagent chat.
func (*Parser) StreamSelected(
	path string,
	selector string,
	opts conversation.LoadOptions,
) iter.Seq2[transcript.Message, error] {
	return func(yield func(transcript.Message, error) bool) {
		if opts.IncludeToolOutputs {
			streamWithToolOutputs(path, selector, opts, yield)
			return
		}
		_, err := readCompleteEvents(path, 0, 0, func(item event) bool {
			if item.Ephemeral || item.AgentID != selector {
				return true
			}
			message, ok := mapEvent(item, opts)
			if ok {
				return yield(message, nil)
			}
			return true
		})
		if err != nil {
			yield(emptyMessage(), err)
		}
	}
}

func streamWithToolOutputs(
	path string,
	selector string,
	opts conversation.LoadOptions,
	yield func(transcript.Message, error) bool,
) {
	events := make([]event, 0)
	_, err := readCompleteEvents(path, 0, 0, func(item event) bool {
		if !item.Ephemeral && item.AgentID == selector {
			events = append(events, item)
		}
		return true
	})
	if err != nil {
		yield(emptyMessage(), err)
		return
	}
	messages := make([]transcript.Message, 0, len(events))
	locations := make(map[string][]toolLocation)
	for _, item := range events {
		message, ok := mapEvent(item, opts)
		if !ok {
			continue
		}
		messageIndex := len(messages)
		messages = append(messages, message)
		for toolIndex, tool := range message.Tools {
			locations[tool.ID] = append(locations[tool.ID], toolLocation{
				messageIndex: messageIndex,
				toolIndex:    toolIndex,
			})
		}
	}
	attachToolOutputs(messages, events, locations)
	for _, message := range messages {
		if !yield(message, nil) {
			return
		}
	}
}

type toolLocation struct {
	messageIndex int
	toolIndex    int
}

func mapEvent(item event, opts conversation.LoadOptions) (transcript.Message, bool) {
	switch item.Type {
	case eventUserMessage:
		return mapUserMessage(item, opts)
	case eventAssistantMessage:
		return mapAssistantMessage(item)
	case eventAssistantReasoning:
		return mapAssistantReasoning(item)
	case eventSystemMessage:
		return mapSystemMessage(item, opts)
	case eventCompactionComplete:
		return mapCompaction(item)
	case eventToolExecutionComplete, eventSessionStart, eventSubagentStarted:
		return emptyMessage(), false
	default:
		return emptyMessage(), false
	}
}

func mapUserMessage(item event, opts conversation.LoadOptions) (transcript.Message, bool) {
	var data userMessageData
	if json.Unmarshal(item.Data, &data) != nil {
		return emptyMessage(), false
	}
	if isSkillSource(data.Source) && !opts.IncludeInjected {
		if opts.HarnessTally != nil {
			opts.HarnessTally.Injected++
		}
		return emptyMessage(), false
	}
	attachments := mapAttachments(data.Attachments)
	if data.Content == "" && len(attachments) == 0 {
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID: item.ID, ParentUUID: parentID(item), LogicalParentUUID: "", Role: "user",
		Visibility: transcript.MessageVisibilityVisible, Compaction: nil,
		Timestamp: parseTime(item.Timestamp), Text: data.Content, Thinking: "",
		HasTools: false, Tools: nil, Attachments: attachments,
	}, true
}

func isSkillSource(source string) bool {
	return strings.HasPrefix(strings.TrimSpace(source), "skill-")
}

func mapAssistantMessage(item event) (transcript.Message, bool) {
	var data assistantMessageData
	if json.Unmarshal(item.Data, &data) != nil {
		return emptyMessage(), false
	}
	tools := make([]transcript.ToolCall, 0, len(data.ToolRequests))
	for _, request := range data.ToolRequests {
		tools = append(tools, transcript.ToolCall{
			ID:          request.ToolCallID,
			Name:        request.Name,
			Input:       transcript.ToolInputJSON{Raw: append(json.RawMessage(nil), request.Arguments...)},
			Display:     firstNonEmpty(request.IntentionSummary, request.ToolTitle),
			DisplayLang: "",
			Output:      "",
			IsError:     false,
			Attachments: nil,
		})
	}
	message := transcript.Message{
		UUID: item.ID, ParentUUID: parentID(item), LogicalParentUUID: "", Role: "assistant",
		Visibility: transcript.MessageVisibilityVisible, Compaction: nil,
		Timestamp: parseTime(item.Timestamp), Text: data.Content, Thinking: data.ReasoningText,
		HasTools: len(tools) > 0, Tools: tools, Attachments: nil,
	}
	return message, data.Content != "" || data.ReasoningText != "" || len(tools) > 0
}

func mapAssistantReasoning(item event) (transcript.Message, bool) {
	var data struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(item.Data, &data) != nil || data.Content == "" {
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID: item.ID, ParentUUID: parentID(item), LogicalParentUUID: "", Role: "assistant",
		Visibility: transcript.MessageVisibilityTranscriptOnly, Compaction: nil,
		Timestamp: parseTime(item.Timestamp), Text: "", Thinking: data.Content,
		HasTools: false, Tools: nil, Attachments: nil,
	}, true
}

func mapSystemMessage(item event, opts conversation.LoadOptions) (transcript.Message, bool) {
	var data struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(item.Data, &data) != nil || data.Content == "" {
		return emptyMessage(), false
	}
	if !opts.IncludeSystemPrompts {
		if opts.HarnessTally != nil {
			opts.HarnessTally.System++
		}
		return emptyMessage(), false
	}
	return transcript.Message{
		UUID: item.ID, ParentUUID: parentID(item), LogicalParentUUID: "", Role: "system",
		Visibility: transcript.MessageVisibilityMetaOnly, Compaction: nil,
		Timestamp: parseTime(item.Timestamp), Text: data.Content, Thinking: "",
		HasTools: false, Tools: nil, Attachments: nil,
	}, true
}

func mapCompaction(item event) (transcript.Message, bool) {
	var data struct {
		Success                     bool                    `json:"success"`
		SummaryContent              string                  `json:"summaryContent"`
		PreCompactionTokens         int                     `json:"preCompactionTokens"`
		PostCompactionTokens        int                     `json:"postCompactionTokens"`
		TokensRemoved               int                     `json:"tokensRemoved"`
		PreCompactionMessagesLength int                     `json:"preCompactionMessagesLength"`
		Trigger                     compactionSourceTrigger `json:"trigger"`
	}
	if json.Unmarshal(item.Data, &data) != nil || !data.Success || data.SummaryContent == "" {
		return emptyMessage(), false
	}
	metadata := &transcript.CompactionMetadata{
		Kind:                    transcript.CompactionKindSummary,
		Trigger:                 compactionTrigger(data.Trigger),
		PreTokens:               data.PreCompactionTokens,
		PostTokens:              data.PostCompactionTokens,
		TokensSaved:             data.TokensRemoved,
		MessagesSummarized:      data.PreCompactionMessagesLength,
		ReplacementHistoryCount: 0,
		HeadUUID:                "", AnchorUUID: "", TailUUID: "", ContextItems: nil,
		UserContext: "", Direction: "", PreCompactDiscoveredTools: nil,
		CompactedToolIDs: nil, ClearedAttachmentUUIDs: nil,
		RawCompactMetadata: nil, RawMicrocompactMetadata: nil, RawSummarizeMetadata: nil,
	}
	return transcript.Message{
		UUID: item.ID, ParentUUID: parentID(item), LogicalParentUUID: "", Role: "assistant",
		Visibility: transcript.MessageVisibilityTranscriptOnly, Compaction: metadata,
		Timestamp: parseTime(item.Timestamp), Text: data.SummaryContent, Thinking: "",
		HasTools: false, Tools: nil, Attachments: nil,
	}, true
}

func compactionTrigger(raw compactionSourceTrigger) transcript.CompactionTrigger {
	switch raw {
	case compactionSourceTriggerManual:
		return transcript.CompactionTriggerManual
	case compactionSourceTriggerThreshold, compactionSourceTriggerAuto:
		return transcript.CompactionTriggerAuto
	default:
		return transcript.CompactionTriggerUnknown
	}
}

func attachToolOutputs(
	messages []transcript.Message,
	events []event,
	locations map[string][]toolLocation,
) {
	for _, item := range events {
		if item.Type != eventToolExecutionComplete {
			continue
		}
		var data toolCompletionData
		if json.Unmarshal(item.Data, &data) != nil || data.ToolCallID == "" {
			continue
		}
		output := firstNonEmpty(data.Result.DetailedContent, data.Result.Content)
		if !data.Success {
			output = data.Error.Message
		}
		attachments := mapAttachments(data.Result.BinaryResultsForLLM)
		for _, location := range locations[data.ToolCallID] {
			tool := &messages[location.messageIndex].Tools[location.toolIndex]
			tool.Output = output
			tool.IsError = !data.Success
			tool.Attachments = attachments
		}
	}
}

func parentID(item event) string {
	if item.ParentID == nil {
		return ""
	}
	return *item.ParentID
}

func emptyMessage() transcript.Message {
	return transcript.Message{
		UUID: "", ParentUUID: "", LogicalParentUUID: "", Role: "",
		Visibility: "", Compaction: nil, Timestamp: parseTime(""), Text: "",
		Thinking: "", HasTools: false, Tools: nil, Attachments: nil,
	}
}
