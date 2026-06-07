package transcript

import "encoding/json"

// CompactedContextItemKind is part of Clyde's typed adapter surface.
type CompactedContextItemKind string

const (
	// CompactedContextItemKindMessage preserves a compacted message item.
	CompactedContextItemKindMessage CompactedContextItemKind = "message"
	// CompactedContextItemKindReasoning preserves a compacted reasoning item.
	CompactedContextItemKindReasoning CompactedContextItemKind = "reasoning"
	// CompactedContextItemKindLocalShellCall preserves a compacted shell call item.
	CompactedContextItemKindLocalShellCall CompactedContextItemKind = "local_shell_call"
	// CompactedContextItemKindFunctionCall preserves a compacted function call item.
	CompactedContextItemKindFunctionCall CompactedContextItemKind = "function_call"
	// CompactedContextItemKindToolSearchCall preserves a compacted tool-search call item.
	CompactedContextItemKindToolSearchCall CompactedContextItemKind = "tool_search_call"
	// CompactedContextItemKindFunctionCallOutput preserves a compacted function-call output item.
	CompactedContextItemKindFunctionCallOutput CompactedContextItemKind = "function_call_output"
	// CompactedContextItemKindCustomToolCall preserves a compacted custom-tool call item.
	CompactedContextItemKindCustomToolCall CompactedContextItemKind = "custom_tool_call"
	// CompactedContextItemKindCustomToolCallOutput preserves a compacted custom-tool output item.
	CompactedContextItemKindCustomToolCallOutput CompactedContextItemKind = "custom_tool_call_output"
	// CompactedContextItemKindToolSearchOutput preserves a compacted tool-search output item.
	CompactedContextItemKindToolSearchOutput CompactedContextItemKind = "tool_search_output"
	// CompactedContextItemKindWebSearchCall preserves a compacted web-search call item.
	CompactedContextItemKindWebSearchCall CompactedContextItemKind = "web_search_call"
	// CompactedContextItemKindImageGenerationCall preserves a compacted image-generation call item.
	CompactedContextItemKindImageGenerationCall CompactedContextItemKind = "image_generation_call"
	// CompactedContextItemKindCompaction preserves a nested compaction item.
	CompactedContextItemKindCompaction CompactedContextItemKind = "compaction"
	// CompactedContextItemKindCompactionTrigger preserves a compaction-trigger item.
	CompactedContextItemKindCompactionTrigger CompactedContextItemKind = "compaction_trigger"
	// CompactedContextItemKindContextCompaction preserves a context-compaction item.
	CompactedContextItemKindContextCompaction CompactedContextItemKind = "context_compaction"
	// CompactedContextItemKindOther preserves an unrecognized compacted item.
	CompactedContextItemKindOther CompactedContextItemKind = "other"
)

// CompactedMessageClass is part of Clyde's typed adapter surface.
type CompactedMessageClass string

const (
	// CompactedMessageClassOrdinary marks a non-summary compacted message.
	CompactedMessageClassOrdinary CompactedMessageClass = "ordinary"
	// CompactedMessageClassSummary marks a compacted summary message.
	CompactedMessageClassSummary CompactedMessageClass = "summary"
)

// CompactedContextItem is the shared typed union for provider compaction items.
type CompactedContextItem struct {
	Kind                 CompactedContextItemKind           `json:"kind"`
	Message              *CompactedMessageItem              `json:"message,omitempty"`
	Reasoning            *CompactedReasoningItem            `json:"reasoning,omitempty"`
	LocalShellCall       *CompactedLocalShellCallItem       `json:"local_shell_call,omitempty"`
	FunctionCall         *CompactedFunctionCallItem         `json:"function_call,omitempty"`
	ToolSearchCall       *CompactedToolSearchCallItem       `json:"tool_search_call,omitempty"`
	FunctionCallOutput   *CompactedFunctionCallOutputItem   `json:"function_call_output,omitempty"`
	CustomToolCall       *CompactedCustomToolCallItem       `json:"custom_tool_call,omitempty"`
	CustomToolCallOutput *CompactedCustomToolCallOutputItem `json:"custom_tool_call_output,omitempty"`
	ToolSearchOutput     *CompactedToolSearchOutputItem     `json:"tool_search_output,omitempty"`
	WebSearchCall        *CompactedWebSearchCallItem        `json:"web_search_call,omitempty"`
	ImageGenerationCall  *CompactedImageGenerationCallItem  `json:"image_generation_call,omitempty"`
	Compaction           *CompactedCompactionItem           `json:"compaction,omitempty"`
	CompactionTrigger    *CompactedCompactionTriggerItem    `json:"compaction_trigger,omitempty"`
	ContextCompaction    *CompactedContextCompactionItem    `json:"context_compaction,omitempty"`
	Other                *CompactedOtherItem                `json:"other,omitempty"`
}

// CompactedMessageItem preserves a compacted message item.
type CompactedMessageItem struct {
	Role         string                        `json:"role"`
	Phase        string                        `json:"phase"`
	Content      []CompactedMessageContentItem `json:"content,omitempty"`
	ContentRaw   json.RawMessage               `json:"content_raw,omitempty"`
	MessageClass CompactedMessageClass         `json:"message_class"`
	Raw          json.RawMessage               `json:"raw,omitempty"`
}

// CompactedMessageContentItem preserves one compacted message-content item.
type CompactedMessageContentItem struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL string          `json:"image_url"`
	Detail   string          `json:"detail"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

// CompactedReasoningItem preserves a compacted reasoning item.
type CompactedReasoningItem struct {
	Summary          []CompactedReasoningSummary `json:"summary,omitempty"`
	SummaryRaw       json.RawMessage             `json:"summary_raw,omitempty"`
	ContentRaw       json.RawMessage             `json:"content_raw,omitempty"`
	EncryptedContent string                      `json:"encrypted_content"`
	Raw              json.RawMessage             `json:"raw,omitempty"`
}

// CompactedReasoningSummary preserves one reasoning-summary block.
type CompactedReasoningSummary struct {
	Type string          `json:"type"`
	Text string          `json:"text"`
	Raw  json.RawMessage `json:"raw,omitempty"`
}

// CompactedLocalShellCallItem preserves a compacted shell-call item.
type CompactedLocalShellCallItem struct {
	CallID    string          `json:"call_id"`
	Status    string          `json:"status"`
	ActionRaw json.RawMessage `json:"action_raw,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// CompactedFunctionCallItem preserves a compacted function-call item.
type CompactedFunctionCallItem struct {
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments string          `json:"arguments"`
	CallID    string          `json:"call_id"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// CompactedToolSearchCallItem preserves a compacted tool-search call item.
type CompactedToolSearchCallItem struct {
	CallID       string          `json:"call_id"`
	Status       string          `json:"status"`
	Execution    string          `json:"execution"`
	ArgumentsRaw json.RawMessage `json:"arguments_raw,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`
}

// CompactedFunctionCallOutputItem preserves a compacted function-call output item.
type CompactedFunctionCallOutputItem struct {
	CallID    string          `json:"call_id"`
	OutputRaw json.RawMessage `json:"output_raw,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// CompactedCustomToolCallItem preserves a compacted custom-tool call item.
type CompactedCustomToolCallItem struct {
	CallID string          `json:"call_id"`
	Name   string          `json:"name"`
	Input  string          `json:"input"`
	Status string          `json:"status"`
	Raw    json.RawMessage `json:"raw,omitempty"`
}

// CompactedCustomToolCallOutputItem preserves a compacted custom-tool output item.
type CompactedCustomToolCallOutputItem struct {
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	OutputRaw json.RawMessage `json:"output_raw,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// CompactedToolSearchOutputItem preserves a compacted tool-search output item.
type CompactedToolSearchOutputItem struct {
	CallID    string            `json:"call_id"`
	Status    string            `json:"status"`
	Execution string            `json:"execution"`
	ToolsRaw  []json.RawMessage `json:"tools_raw,omitempty"`
	Raw       json.RawMessage   `json:"raw,omitempty"`
}

// CompactedWebSearchCallItem preserves a compacted web-search call item.
type CompactedWebSearchCallItem struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	ActionRaw json.RawMessage `json:"action_raw,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// CompactedImageGenerationCallItem preserves a compacted image-generation call item.
type CompactedImageGenerationCallItem struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	RevisedPrompt string          `json:"revised_prompt"`
	Result        string          `json:"result"`
	Raw           json.RawMessage `json:"raw,omitempty"`
}

// CompactedCompactionItem preserves a compacted nested compaction item.
type CompactedCompactionItem struct {
	EncryptedContent string          `json:"encrypted_content"`
	Raw              json.RawMessage `json:"raw,omitempty"`
}

// CompactedCompactionTriggerItem preserves a compacted compaction-trigger item.
type CompactedCompactionTriggerItem struct {
	Raw json.RawMessage `json:"raw,omitempty"`
}

// CompactedContextCompactionItem preserves a compacted context-compaction item.
type CompactedContextCompactionItem struct {
	EncryptedContent string          `json:"encrypted_content"`
	Raw              json.RawMessage `json:"raw,omitempty"`
}

// CompactedOtherItem preserves any unknown compacted item type.
type CompactedOtherItem struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"raw,omitempty"`
}
