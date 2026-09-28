package openai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

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
	// ResponsesStatusIncomplete marks a turn stopped by a length limit or filter.
	ResponsesStatusIncomplete ResponsesStatus = "incomplete"
	// ResponsesStatusFailed marks a turn that ended in an upstream error.
	ResponsesStatusFailed ResponsesStatus = "failed"
)

// ResponsesOutputItemStatus enumerates the lifecycle states supported by
// Responses message, reasoning, and function-call output items.
type ResponsesOutputItemStatus string

const (
	// ResponsesOutputItemStatusInProgress marks an output item that is still open.
	ResponsesOutputItemStatusInProgress ResponsesOutputItemStatus = "in_progress"
	// ResponsesOutputItemStatusCompleted marks an output item that finished cleanly.
	ResponsesOutputItemStatusCompleted ResponsesOutputItemStatus = "completed"
	// ResponsesOutputItemStatusIncomplete marks an output item stopped before completion.
	ResponsesOutputItemStatusIncomplete ResponsesOutputItemStatus = "incomplete"
)

// responsesObjectType is the constant `object` discriminator on the
// Responses response object.
const responsesObjectType = "response"

// responsesMetadataEmpty is the JSON literal the adapter emits for the
// metadata field when it echoes nothing back. The Responses contract
// renders metadata as a JSON object, so the empty case is `{}` rather
// than null.
var responsesMetadataEmpty = json.RawMessage(`{}`)

// ResponsesResponse encodes the Responses API response object. The adapter
// writes it as the nonstreaming body and as the response member of each
// lifecycle stream event.
//
// The documented contract writes every request echo field and writes JSON
// null for an omitted nullable request field. The compatibility contract
// leaves the echo fields nil and omits them.
type ResponsesResponse struct {
	ID                 string                      `json:"id"`
	Object             string                      `json:"object"`
	CreatedAt          int64                       `json:"created_at"`
	Status             ResponsesStatus             `json:"status"`
	Model              string                      `json:"model"`
	Output             []ResponsesOutputItem       `json:"output"`
	Usage              *ResponsesUsage             `json:"usage,omitempty"`
	IncompleteDetails  *ResponsesIncompleteDetails `json:"incomplete_details"`
	Error              *ResponsesError             `json:"error"`
	Metadata           json.RawMessage             `json:"metadata"`
	Instructions       json.RawMessage             `json:"instructions,omitempty"`
	MaxOutputTokens    json.RawMessage             `json:"max_output_tokens,omitempty"`
	ParallelToolCalls  json.RawMessage             `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID json.RawMessage             `json:"previous_response_id,omitempty"`
	Reasoning          json.RawMessage             `json:"reasoning,omitempty"`
	Store              json.RawMessage             `json:"store,omitempty"`
	Temperature        json.RawMessage             `json:"temperature,omitempty"`
	Text               json.RawMessage             `json:"text,omitempty"`
	ToolChoice         json.RawMessage             `json:"tool_choice,omitempty"`
	Tools              json.RawMessage             `json:"tools,omitempty"`
	TopP               json.RawMessage             `json:"top_p,omitempty"`
	Truncation         json.RawMessage             `json:"truncation,omitempty"`
	User               json.RawMessage             `json:"user,omitempty"`
	Clyde              *ResponsesClyde             `json:"clyde,omitempty"`
}

// ResponsesEcho stores the request values that the documented Response
// object repeats. Each field stores raw JSON. The Responses contract
// repeats the client tool, text, and reasoning objects byte for byte.
type ResponsesEcho struct {
	Instructions       json.RawMessage
	MaxOutputTokens    json.RawMessage
	Metadata           json.RawMessage
	ParallelToolCalls  json.RawMessage
	PreviousResponseID json.RawMessage
	Reasoning          json.RawMessage
	Store              json.RawMessage
	Temperature        json.RawMessage
	Text               json.RawMessage
	ToolChoice         json.RawMessage
	Tools              json.RawMessage
	TopP               json.RawMessage
	Truncation         json.RawMessage
	User               json.RawMessage
}

var jsonNull = json.RawMessage(`null`)

// NewResponsesEcho writes the documented default for each omitted field. It
// always writes store as false because Clyde stores no responses. It always
// writes previous_response_id as null because Clyde rejects a request that
// sets it.
func NewResponsesEcho(rr ResponsesRequest) ResponsesEcho {
	return ResponsesEcho{
		Instructions:       echoString(rr.Instructions),
		MaxOutputTokens:    echoInt(rr.MaxOutputTokens),
		Metadata:           echoRawOr(rr.Metadata, responsesMetadataEmpty),
		ParallelToolCalls:  echoBool(rr.ParallelTools, true),
		PreviousResponseID: jsonNull,
		Reasoning:          echoReasoning(rr.Reasoning),
		Store:              json.RawMessage(`false`),
		Temperature:        echoNumber(rr.Temperature, 1),
		Text:               echoRawOr(rr.Text, json.RawMessage(`{"format":{"type":"text"}}`)),
		ToolChoice:         echoRawOr(rr.ToolChoice, json.RawMessage(`"auto"`)),
		Tools:              echoRawOr(rr.Tools, json.RawMessage(`[]`)),
		TopP:               echoNumber(rr.TopP, 1),
		Truncation:         echoRawOr(echoString(rr.Truncation), json.RawMessage(`"disabled"`)),
		User:               echoString(rr.User),
	}
}

func echoRawOr(raw json.RawMessage, fallback json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return fallback
	}
	return raw
}

func echoString(value *string) json.RawMessage {
	if value == nil {
		return jsonNull
	}
	return json.RawMessage(strconv.Quote(*value))
}

func echoInt(value *int) json.RawMessage {
	if value == nil {
		return jsonNull
	}
	return json.RawMessage(strconv.Itoa(*value))
}

func echoBool(value *bool, fallback bool) json.RawMessage {
	selected := fallback
	if value != nil {
		selected = *value
	}
	return json.RawMessage(strconv.FormatBool(selected))
}

func echoNumber(value *float64, fallback float64) json.RawMessage {
	selected := fallback
	if value != nil {
		selected = *value
	}
	return json.RawMessage(strconv.FormatFloat(selected, 'f', -1, 64))
}

func echoReasoning(reasoning *Reasoning) json.RawMessage {
	if reasoning == nil {
		return jsonNull
	}
	encoded, err := json.Marshal(reasoning)
	if err != nil {
		slog.Warn("adapter.openai.responses_reasoning_echo_failed", "concern", "adapter.chat.render", "err", err)
		return jsonNull
	}
	return encoded
}

func applyResponsesEcho(resp *ResponsesResponse, echo ResponsesEcho) {
	resp.Instructions = echo.Instructions
	resp.MaxOutputTokens = echo.MaxOutputTokens
	resp.Metadata = echo.Metadata
	resp.ParallelToolCalls = echo.ParallelToolCalls
	resp.PreviousResponseID = echo.PreviousResponseID
	resp.Reasoning = echo.Reasoning
	resp.Store = echo.Store
	resp.Temperature = echo.Temperature
	resp.Text = echo.Text
	resp.ToolChoice = echo.ToolChoice
	resp.Tools = echo.Tools
	resp.TopP = echo.TopP
	resp.Truncation = echo.Truncation
	resp.User = echo.User
}

// ResponsesClyde carries Clyde-specific extension data on the Responses
// response object. It is omitted entirely when there are no warnings, so a
// warning-free response stays byte-identical to the base OpenAI contract.
type ResponsesClyde struct {
	Warnings []adaptercompat.CompatibilityWarning `json:"warnings,omitempty"`
}

// ResponsesIncompleteDetails carries the reason a turn was incomplete.
// Completed responses render null through the nil pointer field. Incomplete
// responses carry max_output_tokens or content_filter from the finish reason.
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
	Type      string                    `json:"type"`
	ID        string                    `json:"id"`
	Status    ResponsesOutputItemStatus `json:"status,omitempty"`
	Role      string                    `json:"role,omitempty"`
	Content   []ResponsesContentPart    `json:"content,omitempty"`
	Summary   []ResponsesSummaryPart    `json:"summary,omitempty"`
	CallID    string                    `json:"call_id,omitempty"`
	Name      string                    `json:"name,omitempty"`
	Arguments string                    `json:"arguments,omitempty"`
}

// responsesMessageItemWire is the exact JSON shape of a message output
// item. Content is never omitempty so an in-progress message renders
// `"content":[]`.
type responsesMessageItemWire struct {
	Type    string                    `json:"type"`
	ID      string                    `json:"id"`
	Status  ResponsesOutputItemStatus `json:"status"`
	Role    string                    `json:"role"`
	Content []ResponsesContentPart    `json:"content"`
}

// responsesReasoningItemWire is the exact JSON shape of a reasoning
// output item.
type responsesReasoningItemWire struct {
	Type    string                    `json:"type"`
	ID      string                    `json:"id"`
	Status  ResponsesOutputItemStatus `json:"status"`
	Summary []ResponsesSummaryPart    `json:"summary"`
}

// responsesFunctionCallItemWire is the exact JSON shape of a
// function_call output item.
type responsesFunctionCallItemWire struct {
	Type      string                    `json:"type"`
	ID        string                    `json:"id"`
	CallID    string                    `json:"call_id"`
	Name      string                    `json:"name"`
	Arguments string                    `json:"arguments"`
	Status    ResponsesOutputItemStatus `json:"status"`
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
			Status:  i.Status,
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

// ResponsesContentPart is one content part inside a message output item.
// Its marshal implementation permits only output_text and refusal parts.
type ResponsesContentPart struct {
	Type        string                `json:"type"`
	Text        string                `json:"text"`
	Refusal     string                `json:"refusal"`
	Annotations []ResponsesAnnotation `json:"annotations"`
}

type responsesContentPartKind string

const (
	responsesContentPartOutputText responsesContentPartKind = "output_text"
	responsesContentPartRefusal    responsesContentPartKind = "refusal"
)

type responsesOutputTextPartWire struct {
	Type        string                `json:"type"`
	Text        string                `json:"text"`
	Annotations []ResponsesAnnotation `json:"annotations"`
}

type responsesRefusalPartWire struct {
	Type    string `json:"type"`
	Refusal string `json:"refusal"`
}

type responsesContentPartWire interface {
	isResponsesContentPartWire()
}

func (responsesOutputTextPartWire) isResponsesContentPartWire() {}
func (responsesRefusalPartWire) isResponsesContentPartWire()    {}

// MarshalJSON emits the closed content-part union shape selected by Type.
func (p ResponsesContentPart) MarshalJSON() ([]byte, error) {
	switch responsesContentPartKind(p.Type) {
	case responsesContentPartOutputText:
		annotations := p.Annotations
		if annotations == nil {
			annotations = []ResponsesAnnotation{}
		}
		return marshalResponsesContentPartWire(p.Type, responsesOutputTextPartWire{
			Type:        "output_text",
			Text:        p.Text,
			Annotations: annotations,
		})
	case responsesContentPartRefusal:
		return marshalResponsesContentPartWire(p.Type, responsesRefusalPartWire{Type: "refusal", Refusal: p.Refusal})
	default:
		return nil, fmt.Errorf("unsupported responses content part type %q", p.Type)
	}
}

func marshalResponsesContentPartWire(partType string, wire responsesContentPartWire) ([]byte, error) {
	b, err := json.Marshal(wire)
	if err != nil {
		slog.Warn("adapter.openai.responses_content_part_marshal_failed", "concern", "adapter.chat.render", "part_type", partType, "err", err)
		return nil, fmt.Errorf("marshal responses %s content part: %w", partType, err)
	}
	return b, nil
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

// ResponsesUsage omits a details object that the provider did not report
// instead of writing a zero count.
type ResponsesUsage struct {
	InputTokens         int                           `json:"input_tokens"`
	OutputTokens        int                           `json:"output_tokens"`
	TotalTokens         int                           `json:"total_tokens"`
	InputTokensDetails  *ResponsesInputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *ResponsesOutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// ResponsesInputTokensDetails leaves CacheWriteTokens nil when the provider
// omits cache writes.
type ResponsesInputTokensDetails struct {
	CachedTokens     int  `json:"cached_tokens"`
	CacheWriteTokens *int `json:"cache_write_tokens,omitempty"`
}

// ResponsesOutputTokensDetails encodes usage.output_tokens_details.
type ResponsesOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ResponsesUsageFromChat copies prompt_tokens to input_tokens,
// completion_tokens to output_tokens, prompt token details to
// input_tokens_details, and completion token details to
// output_tokens_details. It omits each detail that the provider did not
// report.
func ResponsesUsageFromChat(usage Usage) ResponsesUsage {
	var inputDetails *ResponsesInputTokensDetails
	if usage.PromptTokensDetails != nil {
		inputDetails = &ResponsesInputTokensDetails{
			CachedTokens:     usage.PromptTokensDetails.CachedTokens,
			CacheWriteTokens: usage.PromptTokensDetails.CacheWriteTokens,
		}
	}
	var outputDetails *ResponsesOutputTokensDetails
	if usage.CompletionTokensDetails != nil {
		outputDetails = &ResponsesOutputTokensDetails{ReasoningTokens: usage.CompletionTokensDetails.ReasoningTokens}
	}
	return ResponsesUsage{
		InputTokens:         usage.PromptTokens,
		OutputTokens:        usage.CompletionTokens,
		TotalTokens:         usage.TotalTokens,
		InputTokensDetails:  inputDetails,
		OutputTokensDetails: outputDetails,
	}
}

// CompatibilityResponsesUsage always writes input_tokens_details with the
// cached count and output_tokens_details with reasoning_tokens set to 0. The
// Cursor listener sends this usage.
func CompatibilityResponsesUsage(usage Usage) ResponsesUsage {
	return ResponsesUsage{
		InputTokens:         usage.PromptTokens,
		OutputTokens:        usage.CompletionTokens,
		TotalTokens:         usage.TotalTokens,
		InputTokensDetails:  &ResponsesInputTokensDetails{CachedTokens: usage.CachedTokens(), CacheWriteTokens: nil},
		OutputTokensDetails: &ResponsesOutputTokensDetails{ReasoningTokens: 0},
	}
}

// ResponsesResponseParams carries the assembled turn content the
// builder projects into a Responses response object. Output preserves the
// normalized event order when it is supplied by a renderer.
type ResponsesResponseParams struct {
	ID         string
	Model      string
	CreatedAt  int64
	Status     ResponsesStatus
	Text       string
	Reasoning  string
	Refusal    string
	ToolCalls  []ToolCall
	Output     []ResponsesOutputItem
	Usage      *Usage
	ItemIDBase string
	Warnings   []adaptercompat.CompatibilityWarning
	// A nil Echo omits the request echo fields.
	Echo *ResponsesEcho
	// DocumentedUsage selects ResponsesUsageFromChat when true and
	// CompatibilityResponsesUsage when false.
	DocumentedUsage bool
}

// BuildResponsesResponse assembles a Responses response object from the
// collected turn content. When Output is nil, it derives typed reasoning,
// message, refusal, and function-call items from the collected fields.
func BuildResponsesResponse(params ResponsesResponseParams) ResponsesResponse {
	output := params.Output
	if output == nil {
		output = buildResponsesOutput(params)
	}

	var usage *ResponsesUsage
	if params.Usage != nil {
		mapped := CompatibilityResponsesUsage(*params.Usage)
		if params.DocumentedUsage {
			mapped = ResponsesUsageFromChat(*params.Usage)
		}
		usage = &mapped
	}

	var clyde *ResponsesClyde
	if len(params.Warnings) > 0 {
		clyde = &ResponsesClyde{Warnings: params.Warnings}
	}

	incompleteDetails := responsesIncompleteDetails(params.Status, "")
	resp := ResponsesResponse{
		ID:                 params.ID,
		Object:             responsesObjectType,
		CreatedAt:          params.CreatedAt,
		Status:             params.Status,
		Model:              params.Model,
		Output:             output,
		Usage:              usage,
		IncompleteDetails:  incompleteDetails,
		Error:              nil,
		Metadata:           responsesMetadataEmpty,
		Instructions:       nil,
		MaxOutputTokens:    nil,
		ParallelToolCalls:  nil,
		PreviousResponseID: nil,
		Reasoning:          nil,
		Store:              nil,
		Temperature:        nil,
		Text:               nil,
		ToolChoice:         nil,
		Tools:              nil,
		TopP:               nil,
		Truncation:         nil,
		User:               nil,
		Clyde:              clyde,
	}
	if params.Echo != nil {
		applyResponsesEcho(&resp, *params.Echo)
	}
	return resp
}

func buildResponsesOutput(params ResponsesResponseParams) []ResponsesOutputItem {
	output := make([]ResponsesOutputItem, 0, 2+len(params.ToolCalls))
	itemStatus := responsesOutputItemStatus(params.Status)

	if params.Reasoning != "" {
		output = append(output, ResponsesOutputItem{
			Type:      "reasoning",
			ID:        responsesReasoningItemID(params.ItemIDBase),
			Status:    itemStatus,
			Role:      "",
			Content:   nil,
			Summary:   []ResponsesSummaryPart{{Type: "summary_text", Text: params.Reasoning}},
			CallID:    "",
			Name:      "",
			Arguments: "",
		})
	}

	if params.Text != "" || params.Refusal != "" {
		content := make([]ResponsesContentPart, 0, 2)
		if params.Text != "" {
			content = append(content, ResponsesContentPart{
				Type:        "output_text",
				Text:        params.Text,
				Refusal:     "",
				Annotations: []ResponsesAnnotation{},
			})
		}
		if params.Refusal != "" {
			content = append(content, ResponsesContentPart{
				Type:        "refusal",
				Text:        "",
				Refusal:     params.Refusal,
				Annotations: nil,
			})
		}
		output = append(output, ResponsesOutputItem{
			Type:      "message",
			ID:        responsesMessageItemID(params.ItemIDBase),
			Status:    itemStatus,
			Role:      "assistant",
			Content:   content,
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
			Status:    itemStatus,
			Role:      "",
			Content:   nil,
			Summary:   nil,
			CallID:    responsesCallID(params.ItemIDBase, tc),
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	return output
}

func responsesOutputItemStatus(status ResponsesStatus) ResponsesOutputItemStatus {
	if status == ResponsesStatusIncomplete {
		return ResponsesOutputItemStatusIncomplete
	}
	if status == ResponsesStatusInProgress {
		return ResponsesOutputItemStatusInProgress
	}
	return ResponsesOutputItemStatusCompleted
}

// ResponsesTerminalForFinishReason maps normalized provider finish reasons
// to the Responses terminal status and incomplete details.
func ResponsesTerminalForFinishReason(finishReason string) (ResponsesStatus, *ResponsesIncompleteDetails) {
	trimmed := strings.TrimSpace(finishReason)
	if trimmed == "length" || trimmed == "content_filter" {
		return ResponsesStatusIncomplete, responsesIncompleteDetails(ResponsesStatusIncomplete, trimmed)
	}
	return ResponsesStatusCompleted, nil
}

func responsesIncompleteDetails(status ResponsesStatus, finishReason string) *ResponsesIncompleteDetails {
	if status != ResponsesStatusIncomplete && finishReason != "length" && finishReason != "content_filter" {
		return nil
	}
	reason := "max_output_tokens"
	if strings.TrimSpace(finishReason) == "content_filter" {
		reason = "content_filter"
	}
	return &ResponsesIncompleteDetails{Reason: reason}
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

// responsesCallID derives the public call id from Clyde's response base and
// the provider tool-call index.
func responsesCallID(base string, tc ToolCall) string {
	return "call_" + base + "_" + strconv.Itoa(tc.Index)
}
