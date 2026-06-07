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
	Kind                 CompactedContextItemKind
	Message              *CompactedMessageItem
	Reasoning            *CompactedReasoningItem
	LocalShellCall       *CompactedLocalShellCallItem
	FunctionCall         *CompactedFunctionCallItem
	ToolSearchCall       *CompactedToolSearchCallItem
	FunctionCallOutput   *CompactedFunctionCallOutputItem
	CustomToolCall       *CompactedCustomToolCallItem
	CustomToolCallOutput *CompactedCustomToolCallOutputItem
	ToolSearchOutput     *CompactedToolSearchOutputItem
	WebSearchCall        *CompactedWebSearchCallItem
	ImageGenerationCall  *CompactedImageGenerationCallItem
	Compaction           *CompactedCompactionItem
	CompactionTrigger    *CompactedCompactionTriggerItem
	ContextCompaction    *CompactedContextCompactionItem
	Other                *CompactedOtherItem
}

// CompactedMessageItem preserves a compacted message item.
type CompactedMessageItem struct {
	Role         string
	Phase        string
	Content      []CompactedMessageContentItem
	ContentRaw   json.RawMessage
	MessageClass CompactedMessageClass
	Raw          json.RawMessage
}

// CompactedMessageContentItem preserves one compacted message-content item.
type CompactedMessageContentItem struct {
	Type     string
	Text     string
	ImageURL string
	Detail   string
	Raw      json.RawMessage
}

// CompactedReasoningItem preserves a compacted reasoning item.
type CompactedReasoningItem struct {
	Summary          []CompactedReasoningSummary
	SummaryRaw       json.RawMessage
	ContentRaw       json.RawMessage
	EncryptedContent string
	Raw              json.RawMessage
}

// CompactedReasoningSummary preserves one reasoning-summary block.
type CompactedReasoningSummary struct {
	Type string
	Text string
	Raw  json.RawMessage
}

// CompactedLocalShellCallItem preserves a compacted shell-call item.
type CompactedLocalShellCallItem struct {
	CallID    string
	Status    string
	ActionRaw json.RawMessage
	Raw       json.RawMessage
}

// CompactedFunctionCallItem preserves a compacted function-call item.
type CompactedFunctionCallItem struct {
	Name      string
	Namespace string
	Arguments string
	CallID    string
	Raw       json.RawMessage
}

// CompactedToolSearchCallItem preserves a compacted tool-search call item.
type CompactedToolSearchCallItem struct {
	CallID       string
	Status       string
	Execution    string
	ArgumentsRaw json.RawMessage
	Raw          json.RawMessage
}

// CompactedFunctionCallOutputItem preserves a compacted function-call output item.
type CompactedFunctionCallOutputItem struct {
	CallID    string
	OutputRaw json.RawMessage
	Raw       json.RawMessage
}

// CompactedCustomToolCallItem preserves a compacted custom-tool call item.
type CompactedCustomToolCallItem struct {
	CallID string
	Name   string
	Input  string
	Status string
	Raw    json.RawMessage
}

// CompactedCustomToolCallOutputItem preserves a compacted custom-tool output item.
type CompactedCustomToolCallOutputItem struct {
	CallID    string
	Name      string
	OutputRaw json.RawMessage
	Raw       json.RawMessage
}

// CompactedToolSearchOutputItem preserves a compacted tool-search output item.
type CompactedToolSearchOutputItem struct {
	CallID    string
	Status    string
	Execution string
	ToolsRaw  []json.RawMessage
	Raw       json.RawMessage
}

// CompactedWebSearchCallItem preserves a compacted web-search call item.
type CompactedWebSearchCallItem struct {
	ID        string
	Status    string
	ActionRaw json.RawMessage
	Raw       json.RawMessage
}

// CompactedImageGenerationCallItem preserves a compacted image-generation call item.
type CompactedImageGenerationCallItem struct {
	ID            string
	Status        string
	RevisedPrompt string
	Result        string
	Raw           json.RawMessage
}

// CompactedCompactionItem preserves a compacted nested compaction item.
type CompactedCompactionItem struct {
	EncryptedContent string
	Raw              json.RawMessage
}

// CompactedCompactionTriggerItem preserves a compacted compaction-trigger item.
type CompactedCompactionTriggerItem struct {
	Raw json.RawMessage
}

// CompactedContextCompactionItem preserves a compacted context-compaction item.
type CompactedContextCompactionItem struct {
	EncryptedContent string
	Raw              json.RawMessage
}

// CompactedOtherItem preserves any unknown compacted item type.
type CompactedOtherItem struct {
	Type string
	Raw  json.RawMessage
}
