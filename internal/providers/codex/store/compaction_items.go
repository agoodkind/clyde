package codexstore

import (
	"encoding/json"

	"goodkind.io/clyde/internal/transcript"
)

type rawCompactedItemType struct {
	Type compactedResponseItemType `json:"type"`
}

type compactedResponseItemType string

const (
	compactedResponseItemTypeMessage              compactedResponseItemType = "message"
	compactedResponseItemTypeReasoning            compactedResponseItemType = "reasoning"
	compactedResponseItemTypeLocalShellCall       compactedResponseItemType = "local_shell_call"
	compactedResponseItemTypeFunctionCall         compactedResponseItemType = "function_call"
	compactedResponseItemTypeToolSearchCall       compactedResponseItemType = "tool_search_call"
	compactedResponseItemTypeFunctionCallOutput   compactedResponseItemType = "function_call_output"
	compactedResponseItemTypeCustomToolCall       compactedResponseItemType = "custom_tool_call"
	compactedResponseItemTypeCustomToolCallOutput compactedResponseItemType = "custom_tool_call_output"
	compactedResponseItemTypeToolSearchOutput     compactedResponseItemType = "tool_search_output"
	compactedResponseItemTypeWebSearchCall        compactedResponseItemType = "web_search_call"
	compactedResponseItemTypeImageGenerationCall  compactedResponseItemType = "image_generation_call"
	compactedResponseItemTypeCompaction           compactedResponseItemType = "compaction"
	compactedResponseItemTypeCompactionSummary    compactedResponseItemType = "compaction_summary"
	compactedResponseItemTypeCompactionTrigger    compactedResponseItemType = "compaction_trigger"
	compactedResponseItemTypeContextCompaction    compactedResponseItemType = "context_compaction"
)

type rawCompactedMessageItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Phase   string          `json:"phase"`
}

type rawCompactedMessageContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
}

type rawCompactedReasoningItem struct {
	Type             string          `json:"type"`
	Summary          json.RawMessage `json:"summary"`
	Content          json.RawMessage `json:"content"`
	EncryptedContent string          `json:"encrypted_content"`
}

type rawCompactedReasoningSummaryItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type rawCompactedLocalShellCallItem struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	CallID string          `json:"call_id"`
	Status string          `json:"status"`
	Action json.RawMessage `json:"action"`
}

type rawCompactedFunctionCallItem struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
}

type rawCompactedToolSearchCallItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Status    string          `json:"status"`
	Execution string          `json:"execution"`
	Arguments json.RawMessage `json:"arguments"`
}

type rawCompactedFunctionCallOutputItem struct {
	Type   string          `json:"type"`
	CallID string          `json:"call_id"`
	Output json.RawMessage `json:"output"`
}

type rawCompactedCustomToolCallItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
	Status string `json:"status"`
}

type rawCompactedCustomToolCallOutputItem struct {
	Type   string          `json:"type"`
	CallID string          `json:"call_id"`
	Name   string          `json:"name"`
	Output json.RawMessage `json:"output"`
}

type rawCompactedToolSearchOutputItem struct {
	Type      string            `json:"type"`
	CallID    string            `json:"call_id"`
	Status    string            `json:"status"`
	Execution string            `json:"execution"`
	Tools     []json.RawMessage `json:"tools"`
}

type rawCompactedWebSearchCallItem struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Action json.RawMessage `json:"action"`
}

type rawCompactedImageGenerationCallItem struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	Status        string `json:"status"`
	RevisedPrompt string `json:"revised_prompt"`
	Result        string `json:"result"`
}

type rawCompactedCompactionItem struct {
	Type             string `json:"type"`
	EncryptedContent string `json:"encrypted_content"`
}

type rawCompactedContextCompactionItem struct {
	Type             string `json:"type"`
	EncryptedContent string `json:"encrypted_content"`
}

func normalizeCompactedContextItems(
	rawItems []json.RawMessage,
) []transcript.CompactedContextItem {
	if len(rawItems) == 0 {
		return nil
	}
	items := make([]transcript.CompactedContextItem, 0, len(rawItems))
	for _, rawItem := range rawItems {
		items = append(items, normalizeCompactedContextItem(rawItem))
	}
	return items
}

func normalizeCompactedContextItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var itemType rawCompactedItemType
	if err := json.Unmarshal(rawItem, &itemType); err != nil {
		return otherCompactedContextItem("", rawItem)
	}
	switch itemType.Type {
	case compactedResponseItemTypeMessage:
		return normalizeCompactedMessageItem(rawItem)
	case compactedResponseItemTypeReasoning:
		return normalizeCompactedReasoningItem(rawItem)
	case compactedResponseItemTypeLocalShellCall:
		return normalizeCompactedLocalShellCallItem(rawItem)
	case compactedResponseItemTypeFunctionCall:
		return normalizeCompactedFunctionCallItem(rawItem)
	case compactedResponseItemTypeToolSearchCall:
		return normalizeCompactedToolSearchCallItem(rawItem)
	case compactedResponseItemTypeFunctionCallOutput:
		return normalizeCompactedFunctionCallOutputItem(rawItem)
	case compactedResponseItemTypeCustomToolCall:
		return normalizeCompactedCustomToolCallItem(rawItem)
	case compactedResponseItemTypeCustomToolCallOutput:
		return normalizeCompactedCustomToolCallOutputItem(rawItem)
	case compactedResponseItemTypeToolSearchOutput:
		return normalizeCompactedToolSearchOutputItem(rawItem)
	case compactedResponseItemTypeWebSearchCall:
		return normalizeCompactedWebSearchCallItem(rawItem)
	case compactedResponseItemTypeImageGenerationCall:
		return normalizeCompactedImageGenerationCallItem(rawItem)
	case compactedResponseItemTypeCompaction,
		compactedResponseItemTypeCompactionSummary:
		return normalizeCompactedCompactionItem(rawItem)
	case compactedResponseItemTypeCompactionTrigger:
		return normalizeCompactedCompactionTriggerItem(rawItem)
	case compactedResponseItemTypeContextCompaction:
		return normalizeCompactedContextCompactionItem(rawItem)
	default:
		return otherCompactedContextItem(string(itemType.Type), rawItem)
	}
}

func normalizeCompactedMessageItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedMessageItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("message", rawItem)
	}
	messageItem := transcript.CompactedMessageItem{
		Role:         item.Role,
		Phase:        item.Phase,
		Content:      normalizeCompactedMessageContentItems(item.Content),
		ContentRaw:   cloneRawMessage(item.Content),
		MessageClass: transcript.CompactedMessageClassOrdinary,
		Raw:          cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindMessage,
	)
	contextItem.Message = &messageItem
	return contextItem
}

func normalizeCompactedMessageContentItems(
	rawContent json.RawMessage,
) []transcript.CompactedMessageContentItem {
	if len(rawContent) == 0 {
		return nil
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(rawContent, &rawItems); err != nil {
		return nil
	}
	items := make([]transcript.CompactedMessageContentItem, 0, len(rawItems))
	for _, rawItem := range rawItems {
		var item rawCompactedMessageContentItem
		if err := json.Unmarshal(rawItem, &item); err != nil {
			items = append(items, transcript.CompactedMessageContentItem{
				Type:     "",
				Text:     "",
				ImageURL: "",
				Detail:   "",
				Raw:      cloneRawMessage(rawItem),
			})
			continue
		}
		items = append(items, transcript.CompactedMessageContentItem{
			Type:     item.Type,
			Text:     item.Text,
			ImageURL: item.ImageURL,
			Detail:   item.Detail,
			Raw:      cloneRawMessage(rawItem),
		})
	}
	return items
}

func normalizeCompactedReasoningItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedReasoningItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("reasoning", rawItem)
	}
	reasoningItem := transcript.CompactedReasoningItem{
		Summary:          normalizeCompactedReasoningSummaries(item.Summary),
		SummaryRaw:       cloneRawMessage(item.Summary),
		ContentRaw:       cloneRawMessage(item.Content),
		EncryptedContent: item.EncryptedContent,
		Raw:              cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindReasoning,
	)
	contextItem.Reasoning = &reasoningItem
	return contextItem
}

func normalizeCompactedReasoningSummaries(
	rawSummary json.RawMessage,
) []transcript.CompactedReasoningSummary {
	if len(rawSummary) == 0 {
		return nil
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(rawSummary, &rawItems); err != nil {
		return nil
	}
	items := make([]transcript.CompactedReasoningSummary, 0, len(rawItems))
	for _, rawItem := range rawItems {
		var item rawCompactedReasoningSummaryItem
		if err := json.Unmarshal(rawItem, &item); err != nil {
			items = append(items, transcript.CompactedReasoningSummary{
				Type: "",
				Text: "",
				Raw:  cloneRawMessage(rawItem),
			})
			continue
		}
		items = append(items, transcript.CompactedReasoningSummary{
			Type: item.Type,
			Text: item.Text,
			Raw:  cloneRawMessage(rawItem),
		})
	}
	return items
}

func normalizeCompactedLocalShellCallItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedLocalShellCallItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("local_shell_call", rawItem)
	}
	shellItem := transcript.CompactedLocalShellCallItem{
		CallID:    firstNonEmpty(item.CallID, item.ID),
		Status:    item.Status,
		ActionRaw: cloneRawMessage(item.Action),
		Raw:       cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindLocalShellCall,
	)
	contextItem.LocalShellCall = &shellItem
	return contextItem
}

func normalizeCompactedFunctionCallItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedFunctionCallItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("function_call", rawItem)
	}
	functionItem := transcript.CompactedFunctionCallItem{
		Name:      item.Name,
		Namespace: item.Namespace,
		Arguments: item.Arguments,
		CallID:    item.CallID,
		Raw:       cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindFunctionCall,
	)
	contextItem.FunctionCall = &functionItem
	return contextItem
}

func normalizeCompactedToolSearchCallItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedToolSearchCallItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("tool_search_call", rawItem)
	}
	toolSearchItem := transcript.CompactedToolSearchCallItem{
		CallID:       firstNonEmpty(item.CallID, item.ID),
		Status:       item.Status,
		Execution:    item.Execution,
		ArgumentsRaw: cloneRawMessage(item.Arguments),
		Raw:          cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindToolSearchCall,
	)
	contextItem.ToolSearchCall = &toolSearchItem
	return contextItem
}

func normalizeCompactedFunctionCallOutputItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedFunctionCallOutputItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("function_call_output", rawItem)
	}
	outputItem := transcript.CompactedFunctionCallOutputItem{
		CallID:    item.CallID,
		OutputRaw: cloneRawMessage(item.Output),
		Raw:       cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindFunctionCallOutput,
	)
	contextItem.FunctionCallOutput = &outputItem
	return contextItem
}

func normalizeCompactedCustomToolCallItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedCustomToolCallItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("custom_tool_call", rawItem)
	}
	customItem := transcript.CompactedCustomToolCallItem{
		CallID: item.CallID,
		Name:   item.Name,
		Input:  item.Input,
		Status: item.Status,
		Raw:    cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindCustomToolCall,
	)
	contextItem.CustomToolCall = &customItem
	return contextItem
}

func normalizeCompactedCustomToolCallOutputItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedCustomToolCallOutputItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("custom_tool_call_output", rawItem)
	}
	customOutputItem := transcript.CompactedCustomToolCallOutputItem{
		CallID:    item.CallID,
		Name:      item.Name,
		OutputRaw: cloneRawMessage(item.Output),
		Raw:       cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindCustomToolCallOutput,
	)
	contextItem.CustomToolCallOutput = &customOutputItem
	return contextItem
}

func normalizeCompactedToolSearchOutputItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedToolSearchOutputItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("tool_search_output", rawItem)
	}
	tools := make([]json.RawMessage, 0, len(item.Tools))
	for _, tool := range item.Tools {
		tools = append(tools, cloneRawMessage(tool))
	}
	outputItem := transcript.CompactedToolSearchOutputItem{
		CallID:    item.CallID,
		Status:    item.Status,
		Execution: item.Execution,
		ToolsRaw:  tools,
		Raw:       cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindToolSearchOutput,
	)
	contextItem.ToolSearchOutput = &outputItem
	return contextItem
}

func normalizeCompactedWebSearchCallItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedWebSearchCallItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("web_search_call", rawItem)
	}
	webSearchItem := transcript.CompactedWebSearchCallItem{
		ID:        item.ID,
		Status:    item.Status,
		ActionRaw: cloneRawMessage(item.Action),
		Raw:       cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindWebSearchCall,
	)
	contextItem.WebSearchCall = &webSearchItem
	return contextItem
}

func normalizeCompactedImageGenerationCallItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedImageGenerationCallItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("image_generation_call", rawItem)
	}
	imageItem := transcript.CompactedImageGenerationCallItem{
		ID:            item.ID,
		Status:        item.Status,
		RevisedPrompt: item.RevisedPrompt,
		Result:        item.Result,
		Raw:           cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindImageGenerationCall,
	)
	contextItem.ImageGenerationCall = &imageItem
	return contextItem
}

func normalizeCompactedCompactionItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedCompactionItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("compaction", rawItem)
	}
	compactionItem := transcript.CompactedCompactionItem{
		EncryptedContent: item.EncryptedContent,
		Raw:              cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindCompaction,
	)
	contextItem.Compaction = &compactionItem
	return contextItem
}

func normalizeCompactedCompactionTriggerItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	triggerItem := transcript.CompactedCompactionTriggerItem{
		Raw: cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindCompactionTrigger,
	)
	contextItem.CompactionTrigger = &triggerItem
	return contextItem
}

func normalizeCompactedContextCompactionItem(
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	var item rawCompactedContextCompactionItem
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return otherCompactedContextItem("context_compaction", rawItem)
	}
	contextItem := transcript.CompactedContextCompactionItem{
		EncryptedContent: item.EncryptedContent,
		Raw:              cloneRawMessage(rawItem),
	}
	compactedItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindContextCompaction,
	)
	compactedItem.ContextCompaction = &contextItem
	return compactedItem
}

func otherCompactedContextItem(
	itemType string,
	rawItem json.RawMessage,
) transcript.CompactedContextItem {
	otherItem := transcript.CompactedOtherItem{
		Type: itemType,
		Raw:  cloneRawMessage(rawItem),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindOther,
	)
	contextItem.Other = &otherItem
	return contextItem
}

func legacyCompactedSummaryContextItem(
	text string,
) transcript.CompactedContextItem {
	contentRaw, messageRaw := legacyCompactedSummaryRaw(text)
	messageItem := transcript.CompactedMessageItem{
		Role:         "user",
		Phase:        "",
		Content:      []transcript.CompactedMessageContentItem{legacyCompactedSummaryContentItem(text, contentRaw)},
		ContentRaw:   cloneRawMessage(contentRaw),
		MessageClass: transcript.CompactedMessageClassSummary,
		Raw:          cloneRawMessage(messageRaw),
	}
	contextItem := emptyCompactedContextItem(
		transcript.CompactedContextItemKindMessage,
	)
	contextItem.Message = &messageItem
	return contextItem
}

func legacyCompactedSummaryContentItem(
	text string,
	contentRaw json.RawMessage,
) transcript.CompactedMessageContentItem {
	return transcript.CompactedMessageContentItem{
		Type:     "input_text",
		Text:     text,
		ImageURL: "",
		Detail:   "",
		Raw:      cloneRawMessage(contentRaw),
	}
}

func legacyCompactedSummaryRaw(text string) (json.RawMessage, json.RawMessage) {
	contentBytes, err := json.Marshal([]rawCompactedMessageContentItem{{
		Type:     "input_text",
		Text:     text,
		ImageURL: "",
		Detail:   "",
	}})
	if err != nil {
		return nil, nil
	}
	messageBytes, err := json.Marshal(rawCompactedMessageItem{
		Type:    "message",
		Role:    "user",
		Content: json.RawMessage(contentBytes),
		Phase:   "",
	})
	if err != nil {
		return cloneRawMessage(json.RawMessage(contentBytes)), nil
	}
	return cloneRawMessage(json.RawMessage(contentBytes)), cloneRawMessage(json.RawMessage(messageBytes))
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func firstNonEmpty(a string, b string) string {
	if a != "" {
		return a
	}
	return b
}

func emptyCompactedContextItem(
	kind transcript.CompactedContextItemKind,
) transcript.CompactedContextItem {
	return transcript.CompactedContextItem{
		Kind:                 kind,
		Message:              nil,
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
