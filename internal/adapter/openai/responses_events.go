package openai

// Responses streaming event names. Each is used both as the SSE
// `event:` name and as the `type` discriminator inside the frame's JSON
// data, which the Responses contract keeps identical.
const (
	ResponsesEventCreated               = "response.created"
	ResponsesEventInProgress            = "response.in_progress"
	ResponsesEventCompleted             = "response.completed"
	ResponsesEventFailed                = "response.failed"
	ResponsesEventOutputItemAdded       = "response.output_item.added"
	ResponsesEventOutputItemDone        = "response.output_item.done"
	ResponsesEventContentPartAdded      = "response.content_part.added"
	ResponsesEventContentPartDone       = "response.content_part.done"
	ResponsesEventOutputTextDelta       = "response.output_text.delta"
	ResponsesEventOutputTextDone        = "response.output_text.done"
	ResponsesEventReasoningSummaryDelta = "response.reasoning_summary_text.delta"
	ResponsesEventReasoningSummaryDone  = "response.reasoning_summary_text.done"
	ResponsesEventFunctionArgsDelta     = "response.function_call_arguments.delta"
	ResponsesEventFunctionArgsDone      = "response.function_call_arguments.done"
)

// ResponsesStreamEvent is the closed set of typed Responses SSE event
// payloads the streaming writer serializes. It keeps the marshal seam
// typed so callers never pass an open-ended any into the writer.
type ResponsesStreamEvent interface {
	isResponsesStreamEvent()
}

// ResponsesEnvelopeEvent is the frame shape for the lifecycle events
// that carry the whole response object (created, in_progress,
// completed, failed).
type ResponsesEnvelopeEvent struct {
	Type           string            `json:"type"`
	Response       ResponsesResponse `json:"response"`
	SequenceNumber int               `json:"sequence_number"`
}

// ResponsesOutputItemEvent is the frame shape for output_item.added and
// output_item.done.
type ResponsesOutputItemEvent struct {
	Type           string              `json:"type"`
	OutputIndex    int                 `json:"output_index"`
	Item           ResponsesOutputItem `json:"item"`
	SequenceNumber int                 `json:"sequence_number"`
}

// ResponsesContentPartEvent is the frame shape for content_part.added
// and content_part.done.
type ResponsesContentPartEvent struct {
	Type           string               `json:"type"`
	ItemID         string               `json:"item_id"`
	OutputIndex    int                  `json:"output_index"`
	ContentIndex   int                  `json:"content_index"`
	Part           ResponsesContentPart `json:"part"`
	SequenceNumber int                  `json:"sequence_number"`
}

// ResponsesOutputTextDeltaEvent is the frame shape for
// output_text.delta.
type ResponsesOutputTextDeltaEvent struct {
	Type           string `json:"type"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Delta          string `json:"delta"`
	SequenceNumber int    `json:"sequence_number"`
}

// ResponsesOutputTextDoneEvent is the frame shape for output_text.done.
type ResponsesOutputTextDoneEvent struct {
	Type           string `json:"type"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Text           string `json:"text"`
	SequenceNumber int    `json:"sequence_number"`
}

// ResponsesReasoningSummaryDeltaEvent is the frame shape for
// reasoning_summary_text.delta.
type ResponsesReasoningSummaryDeltaEvent struct {
	Type           string `json:"type"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	Delta          string `json:"delta"`
	SequenceNumber int    `json:"sequence_number"`
}

// ResponsesReasoningSummaryDoneEvent is the frame shape for
// reasoning_summary_text.done.
type ResponsesReasoningSummaryDoneEvent struct {
	Type           string `json:"type"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	Text           string `json:"text"`
	SequenceNumber int    `json:"sequence_number"`
}

// ResponsesFunctionArgsDeltaEvent is the frame shape for
// function_call_arguments.delta.
type ResponsesFunctionArgsDeltaEvent struct {
	Type           string `json:"type"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	Delta          string `json:"delta"`
	SequenceNumber int    `json:"sequence_number"`
}

// ResponsesFunctionArgsDoneEvent is the frame shape for
// function_call_arguments.done.
type ResponsesFunctionArgsDoneEvent struct {
	Type           string `json:"type"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	Arguments      string `json:"arguments"`
	SequenceNumber int    `json:"sequence_number"`
}

func (ResponsesEnvelopeEvent) isResponsesStreamEvent()              {}
func (ResponsesOutputItemEvent) isResponsesStreamEvent()            {}
func (ResponsesContentPartEvent) isResponsesStreamEvent()           {}
func (ResponsesOutputTextDeltaEvent) isResponsesStreamEvent()       {}
func (ResponsesOutputTextDoneEvent) isResponsesStreamEvent()        {}
func (ResponsesReasoningSummaryDeltaEvent) isResponsesStreamEvent() {}
func (ResponsesReasoningSummaryDoneEvent) isResponsesStreamEvent()  {}
func (ResponsesFunctionArgsDeltaEvent) isResponsesStreamEvent()     {}
func (ResponsesFunctionArgsDoneEvent) isResponsesStreamEvent()      {}
