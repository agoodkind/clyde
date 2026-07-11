package openai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
)

// ResponsesStatus enumerates the OpenAI Responses API response status
// values the adapter emits on the terminal response object.
type ResponsesStatus string

const (
	// ResponsesStatusInProgress is the status carried by the streamed
	// response.created and response.in_progress objects.
	ResponsesStatusInProgress ResponsesStatus = "in_progress"
	// ResponsesStatusCompleted marks a clean turn completion.
	ResponsesStatusCompleted ResponsesStatus = "completed"
	// ResponsesStatusIncomplete marks a turn truncated by a length limit.
	ResponsesStatusIncomplete ResponsesStatus = "incomplete"
	// ResponsesStatusFailed marks a turn that ended in an upstream error.
	ResponsesStatusFailed ResponsesStatus = "failed"
)

// responsesObjectType is the constant `object` discriminator on the
// Responses response object.
const responsesObjectType = "response"

// responsesMetadataEmpty is the JSON literal the adapter emits for the
// metadata field when it echoes nothing back. The Responses contract
// renders metadata as a JSON object, so the empty case is `{}` rather
// than null.
var responsesMetadataEmpty = json.RawMessage(`{}`)

// ResponsesResponse is the top-level OpenAI Responses API response
// object. The adapter emits it both as the non-streaming body and as
// the `response` payload embedded in the streamed lifecycle events.
type ResponsesResponse struct {
	ID                string                      `json:"id"`
	Object            string                      `json:"object"`
	CreatedAt         int64                       `json:"created_at"`
	Status            ResponsesStatus             `json:"status"`
	Model             string                      `json:"model"`
	Output            []ResponsesOutputItem       `json:"output"`
	Usage             *ResponsesUsage             `json:"usage,omitempty"`
	IncompleteDetails *ResponsesIncompleteDetails `json:"incomplete_details"`
	Error             *ResponsesError             `json:"error"`
	Metadata          json.RawMessage             `json:"metadata"`
	Clyde             *ResponsesClyde             `json:"clyde,omitempty"`
}

// ResponsesClyde carries Clyde-specific extension data on the Responses
// response object. It is omitted entirely when there are no warnings, so a
// warning-free response stays byte-identical to the base OpenAI contract.
type ResponsesClyde struct {
	Warnings []adaptercompat.CompatibilityWarning `json:"warnings,omitempty"`
}

// ResponsesIncompleteDetails carries the reason a turn was truncated.
// The adapter emits null today; the pointer field renders JSON null
// when nil.
type ResponsesIncompleteDetails struct {
	Reason string `json:"reason"`
}

// ResponsesError is the Responses-shaped terminal error object embedded
// on a failed response. Nil renders as JSON null.
type ResponsesError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// ResponsesOutputItem is one item in the Responses output array. It is
// a tagged union over the three item shapes the adapter emits
// (reasoning, message, function_call). MarshalJSON emits only the
// fields that belong to Type so each item matches the Responses wire
// shape exactly.
type ResponsesOutputItem struct {
	Type      string                 `json:"type"`
	ID        string                 `json:"id"`
	Status    string                 `json:"status,omitempty"`
	Role      string                 `json:"role,omitempty"`
	Content   []ResponsesContentPart `json:"content,omitempty"`
	Summary   []ResponsesSummaryPart `json:"summary,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Arguments string                 `json:"arguments,omitempty"`
}

// responsesMessageItemWire is the exact JSON shape of a message output
// item. Content is never omitempty so an in-progress message renders
// `"content":[]`.
type responsesMessageItemWire struct {
	Type    string                 `json:"type"`
	ID      string                 `json:"id"`
	Status  string                 `json:"status"`
	Role    string                 `json:"role"`
	Content []ResponsesContentPart `json:"content"`
}

// responsesReasoningItemWire is the exact JSON shape of a reasoning
// output item.
type responsesReasoningItemWire struct {
	Type    string                 `json:"type"`
	ID      string                 `json:"id"`
	Summary []ResponsesSummaryPart `json:"summary"`
}

// responsesFunctionCallItemWire is the exact JSON shape of a
// function_call output item.
type responsesFunctionCallItemWire struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

// responsesOutputItemKind enumerates the Responses output item type
// discriminators MarshalJSON routes on so each item emits only the
// fields for its shape.
type responsesOutputItemKind string

const (
	responsesItemMessage      responsesOutputItemKind = "message"
	responsesItemReasoning    responsesOutputItemKind = "reasoning"
	responsesItemFunctionCall responsesOutputItemKind = "function_call"
)

// MarshalJSON emits the output item using the wire shape for its Type
// so reasoning, message, and function_call items each carry only their
// own fields.
func (i ResponsesOutputItem) MarshalJSON() ([]byte, error) {
	switch responsesOutputItemKind(i.Type) {
	case responsesItemMessage:
		content := i.Content
		if content == nil {
			content = []ResponsesContentPart{}
		}
		return marshalResponsesItemWire(i.Type, responsesMessageItemWire{
			Type:    "message",
			ID:      i.ID,
			Status:  i.Status,
			Role:    i.Role,
			Content: content,
		})
	case responsesItemReasoning:
		summary := i.Summary
		if summary == nil {
			summary = []ResponsesSummaryPart{}
		}
		return marshalResponsesItemWire(i.Type, responsesReasoningItemWire{
			Type:    "reasoning",
			ID:      i.ID,
			Summary: summary,
		})
	case responsesItemFunctionCall:
		return marshalResponsesItemWire(i.Type, responsesFunctionCallItemWire{
			Type:      "function_call",
			ID:        i.ID,
			CallID:    i.CallID,
			Name:      i.Name,
			Arguments: i.Arguments,
			Status:    i.Status,
		})
	default:
		return nil, fmt.Errorf("unsupported responses output item type %q", i.Type)
	}
}

// marshalResponsesItemWire marshals one output item wire shape and wraps
// any marshal error with the item type for context.
func marshalResponsesItemWire(itemType string, wire responsesOutputItemWire) ([]byte, error) {
	b, err := json.Marshal(wire)
	if err != nil {
		slog.Warn("adapter.openai.responses_item_marshal_failed", "concern", "adapter.chat.render", "item_type", itemType, "err", err)
		return nil, fmt.Errorf("marshal responses %s item: %w", itemType, err)
	}
	return b, nil
}

// responsesOutputItemWire is the closed set of per-type wire shapes
// marshalResponsesItemWire serializes.
type responsesOutputItemWire interface {
	isResponsesOutputItemWire()
}

func (responsesMessageItemWire) isResponsesOutputItemWire()      {}
func (responsesReasoningItemWire) isResponsesOutputItemWire()    {}
func (responsesFunctionCallItemWire) isResponsesOutputItemWire() {}

// ResponsesContentPart is one content part inside a message output
// item. Task A only emits output_text parts.
type ResponsesContentPart struct {
	Type        string                `json:"type"`
	Text        string                `json:"text"`
	Annotations []ResponsesAnnotation `json:"annotations"`
}

// ResponsesAnnotation is a placeholder for output_text annotations.
// Task A never emits annotations, so the annotations array always
// marshals empty; later tasks populate citation and file annotations
// through this typed shape.
type ResponsesAnnotation struct {
	Type string `json:"type"`
}

// ResponsesSummaryPart is one reasoning summary part inside a reasoning
// output item.
type ResponsesSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ResponsesUsage is the Responses-shaped token usage block.
type ResponsesUsage struct {
	InputTokens         int                          `json:"input_tokens"`
	OutputTokens        int                          `json:"output_tokens"`
	TotalTokens         int                          `json:"total_tokens"`
	InputTokensDetails  ResponsesInputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails ResponsesOutputTokensDetails `json:"output_tokens_details"`
}

// ResponsesInputTokensDetails carries the cached-prompt token count.
type ResponsesInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// ResponsesOutputTokensDetails carries the reasoning token count.
type ResponsesOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ResponsesUsageFromChat maps the OpenAI chat Usage the provider reports
// into the Responses usage shape: input=prompt, output=completion,
// total=total, cached from the prompt token details. Reasoning token
// detail is not exposed on the chat Usage struct, so reasoning_tokens
// stays zero for Task A.
func ResponsesUsageFromChat(usage Usage) ResponsesUsage {
	return ResponsesUsage{
		InputTokens:         usage.PromptTokens,
		OutputTokens:        usage.CompletionTokens,
		TotalTokens:         usage.TotalTokens,
		InputTokensDetails:  ResponsesInputTokensDetails{CachedTokens: usage.CachedTokens()},
		OutputTokensDetails: ResponsesOutputTokensDetails{ReasoningTokens: 0},
	}
}

// ResponsesResponseParams carries the assembled turn content the
// builder projects into a Responses response object. Text, Reasoning,
// Refusal, and ToolCalls come from render.CollectMessage on the
// non-streaming path or from the streaming writer's accumulated state.
type ResponsesResponseParams struct {
	ID         string
	Model      string
	CreatedAt  int64
	Status     ResponsesStatus
	Text       string
	Reasoning  string
	Refusal    string
	ToolCalls  []ToolCall
	Usage      *Usage
	ItemIDBase string
	Warnings   []adaptercompat.CompatibilityWarning
}

// BuildResponsesResponse assembles a Responses response object from the
// collected turn content. It emits a reasoning item only when reasoning
// text was produced, a message item only when there is assistant text
// (refusal folded into text for Task A), and one function_call item per
// tool call. Output item ids derive from ItemIDBase and the tool call
// index so the streamed lifecycle events and the terminal object share
// stable ids.
func BuildResponsesResponse(params ResponsesResponseParams) ResponsesResponse {
	output := make([]ResponsesOutputItem, 0, 2+len(params.ToolCalls))

	if params.Reasoning != "" {
		output = append(output, ResponsesOutputItem{
			Type:      "reasoning",
			ID:        responsesReasoningItemID(params.ItemIDBase),
			Status:    "",
			Role:      "",
			Content:   nil,
			Summary:   []ResponsesSummaryPart{{Type: "summary_text", Text: params.Reasoning}},
			CallID:    "",
			Name:      "",
			Arguments: "",
		})
	}

	// TODO(responses-refusal): a later task renders refusals as a
	// dedicated refusal content part; Task A folds refusal text into the
	// assistant output_text so no content is lost.
	messageText := params.Text + params.Refusal
	if messageText != "" {
		output = append(output, ResponsesOutputItem{
			Type:   "message",
			ID:     responsesMessageItemID(params.ItemIDBase),
			Status: "completed",
			Role:   "assistant",
			Content: []ResponsesContentPart{{
				Type:        "output_text",
				Text:        messageText,
				Annotations: []ResponsesAnnotation{},
			}},
			Summary:   nil,
			CallID:    "",
			Name:      "",
			Arguments: "",
		})
	}

	for _, tc := range params.ToolCalls {
		output = append(output, ResponsesOutputItem{
			Type:      "function_call",
			ID:        responsesFunctionCallItemID(params.ItemIDBase, tc.Index),
			Status:    "completed",
			Role:      "",
			Content:   nil,
			Summary:   nil,
			CallID:    responsesCallID(params.ItemIDBase, tc),
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	var usage *ResponsesUsage
	if params.Usage != nil {
		mapped := ResponsesUsageFromChat(*params.Usage)
		usage = &mapped
	}

	var clyde *ResponsesClyde
	if len(params.Warnings) > 0 {
		clyde = &ResponsesClyde{Warnings: params.Warnings}
	}

	return ResponsesResponse{
		ID:                params.ID,
		Object:            responsesObjectType,
		CreatedAt:         params.CreatedAt,
		Status:            params.Status,
		Model:             params.Model,
		Output:            output,
		Usage:             usage,
		IncompleteDetails: nil,
		Error:             nil,
		Metadata:          responsesMetadataEmpty,
		Clyde:             clyde,
	}
}

// responsesReasoningItemID derives the reasoning item id from the base.
func responsesReasoningItemID(base string) string {
	return "rs_" + base
}

// responsesMessageItemID derives the message item id from the base.
func responsesMessageItemID(base string) string {
	return "msg_" + base
}

// responsesFunctionCallItemID derives the function_call item id from the
// base and the tool call index.
func responsesFunctionCallItemID(base string, index int) string {
	return "fc_" + base + "_" + strconv.Itoa(index)
}

// responsesCallID returns the upstream tool call id when present, or a
// stable synthesized call id derived from the base and index.
func responsesCallID(base string, tc ToolCall) string {
	if tc.ID != "" {
		return tc.ID
	}
	return "call_" + base + "_" + strconv.Itoa(tc.Index)
}
