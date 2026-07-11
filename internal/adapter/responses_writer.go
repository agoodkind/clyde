package adapter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/clock"
)

// responsesStreamWriter implements provider.EventWriter for the OpenAI
// Responses streaming path. It consumes the same normalized render
// events the chat writer consumes, but translates them into the
// Responses named-event SSE sequence (reasoning item, then message
// item, then function_call items) instead of chat completion chunks.
// It never emits a `data: [DONE]` terminator; the terminal frame is
// response.completed (or response.failed).
type responsesStreamWriter struct {
	sse        *adapteropenai.SSEWriter
	flusher    http.Flusher
	log        *slog.Logger
	responseID string
	model      string
	itemBase   string
	createdAt  int64

	began           bool
	seq             int
	nextOutputIndex int

	reasoningOpen        bool
	reasoningItemID      string
	reasoningOutputIndex int
	reasoningText        strings.Builder

	messageOpen        bool
	messageItemID      string
	messageOutputIndex int
	messageText        strings.Builder

	toolStates map[int]*responsesStreamToolState
	toolOrder  []int
}

// responsesStreamToolState tracks one streamed function_call output
// item across its argument deltas.
type responsesStreamToolState struct {
	itemID      string
	callID      string
	name        string
	outputIndex int
	index       int
	args        strings.Builder
}

// newResponsesStreamWriter builds a Responses streaming writer over the
// response writer. The responseID is the stable resp_ id allocated at
// handler entry; item ids derive from it so the streamed events and the
// terminal object share ids.
func newResponsesStreamWriter(w http.ResponseWriter, responseID, model string, log *slog.Logger) (*responsesStreamWriter, error) {
	sse, err := adapteropenai.NewSSEWriter(w)
	if err != nil {
		if log != nil {
			log.Warn("adapter.responses.new_sse_failed", "concern", "adapter.chat.render", "response_id", responseID, "model", model, "err", err)
		}
		return nil, fmt.Errorf("create responses SSE writer: %w", err)
	}
	flusher, _ := w.(http.Flusher)
	return &responsesStreamWriter{
		sse:                  sse,
		flusher:              flusher,
		log:                  log,
		responseID:           responseID,
		model:                model,
		itemBase:             responsesItemBase(responseID),
		createdAt:            clock.Now().Unix(),
		began:                false,
		seq:                  0,
		nextOutputIndex:      0,
		reasoningOpen:        false,
		reasoningItemID:      "",
		reasoningOutputIndex: 0,
		reasoningText:        strings.Builder{},
		messageOpen:          false,
		messageItemID:        "",
		messageOutputIndex:   0,
		messageText:          strings.Builder{},
		toolStates:           make(map[int]*responsesStreamToolState),
		toolOrder:            nil,
	}, nil
}

// responsesItemBase strips the resp_ prefix so item ids read
// msg_<base>, rs_<base>, and fc_<base>_<n>.
func responsesItemBase(responseID string) string {
	return strings.TrimPrefix(responseID, "resp_")
}

func (p *responsesStreamWriter) nextSeq() int {
	seq := p.seq
	p.seq++
	return seq
}

func (p *responsesStreamWriter) send(name string, payload []byte) error {
	if err := p.sse.WriteNamedEvent(name, payload); err != nil {
		if p.log != nil {
			p.log.Warn("adapter.responses.emit_failed", "concern", "adapter.chat.render", "event", name, "err", err)
		}
		return fmt.Errorf("write responses named event %q: %w", name, err)
	}
	return nil
}

// begin emits response.created and response.in_progress with the
// in-progress response object. It is idempotent so the dispatch path
// and WriteEvent can both call it safely.
func (p *responsesStreamWriter) begin() error {
	if p.began {
		return nil
	}
	p.began = true
	inProgress := p.buildResponse(adapteropenai.ResponsesStatusInProgress, nil)
	if err := p.emitEnvelope(adapteropenai.ResponsesEventCreated, inProgress); err != nil {
		return err
	}
	return p.emitEnvelope(adapteropenai.ResponsesEventInProgress, inProgress)
}

// WriteEvent translates one normalized render event into the Responses
// SSE sequence.
func (p *responsesStreamWriter) WriteEvent(ev adapterrender.Event) error {
	if err := p.begin(); err != nil {
		return err
	}
	switch e := ev.(type) {
	case adapterrender.ReasoningSignaled:
		// The reasoning item opens on the first reasoning delta so an
		// empty reasoning summary is never emitted.
		return nil
	case adapterrender.ReasoningDelta:
		return p.handleReasoningDelta(e.Text)
	case adapterrender.ReasoningFinished:
		return p.closeReasoning()
	case adapterrender.TextDelta:
		return p.handleText(e.Text)
	case adapterrender.RefusalDelta:
		// Task A folds refusal text into the assistant output_text; a
		// dedicated refusal part is a later task.
		return p.handleText(e.Text)
	case adapterrender.ToolCallDelta:
		return p.handleToolCalls(e.ToolCalls)
	}
	return nil
}

// Flush forces buffered SSE output to the network.
func (p *responsesStreamWriter) Flush() error {
	if p.flusher != nil {
		p.flusher.Flush()
	}
	return nil
}

// finish closes any open reasoning, message, and function_call items,
// then emits response.completed carrying the terminal object and usage.
func (p *responsesStreamWriter) finish(result adapterprovider.Result) error {
	if err := p.begin(); err != nil {
		return err
	}
	if err := p.closeReasoning(); err != nil {
		return err
	}
	if err := p.closeMessage(); err != nil {
		return err
	}
	if err := p.closeTools(); err != nil {
		return err
	}
	status := adapteropenai.ResponsesStatusCompleted
	if result.FinishReason == "length" {
		status = adapteropenai.ResponsesStatusIncomplete
	}
	usage := result.Usage
	final := p.buildResponse(status, &usage)
	return p.emitEnvelope(adapteropenai.ResponsesEventCompleted, final)
}

// fail emits response.failed carrying a failed response object and the
// error message. The full Responses error dialect is a later task.
func (p *responsesStreamWriter) fail(err error) error {
	if beginErr := p.begin(); beginErr != nil {
		return beginErr
	}
	resp := p.buildResponse(adapteropenai.ResponsesStatusFailed, nil)
	resp.Error = &adapteropenai.ResponsesError{Code: "upstream_error", Message: err.Error()}
	return p.emitEnvelope(adapteropenai.ResponsesEventFailed, resp)
}

func (p *responsesStreamWriter) handleReasoningDelta(text string) error {
	if text == "" {
		return nil
	}
	if err := p.openReasoning(); err != nil {
		return err
	}
	p.reasoningText.WriteString(text)
	evt := adapteropenai.ResponsesReasoningSummaryDeltaEvent{
		Type:           adapteropenai.ResponsesEventReasoningSummaryDelta,
		ItemID:         p.reasoningItemID,
		OutputIndex:    p.reasoningOutputIndex,
		SummaryIndex:   0,
		Delta:          text,
		SequenceNumber: p.nextSeq(),
	}
	return p.marshalSend(adapteropenai.ResponsesEventReasoningSummaryDelta, evt)
}

func (p *responsesStreamWriter) openReasoning() error {
	if p.reasoningOpen {
		return nil
	}
	p.reasoningOpen = true
	p.reasoningItemID = "rs_" + p.itemBase
	p.reasoningOutputIndex = p.nextOutputIndex
	p.nextOutputIndex++
	item := adapteropenai.ResponsesOutputItem{
		Type: "reasoning", ID: p.reasoningItemID, Status: "", Role: "",
		Content: nil, Summary: []adapteropenai.ResponsesSummaryPart{}, CallID: "", Name: "", Arguments: "",
	}
	return p.emitOutputItem(adapteropenai.ResponsesEventOutputItemAdded, p.reasoningOutputIndex, item)
}

func (p *responsesStreamWriter) closeReasoning() error {
	if !p.reasoningOpen {
		return nil
	}
	p.reasoningOpen = false
	full := p.reasoningText.String()
	done := adapteropenai.ResponsesReasoningSummaryDoneEvent{
		Type:           adapteropenai.ResponsesEventReasoningSummaryDone,
		ItemID:         p.reasoningItemID,
		OutputIndex:    p.reasoningOutputIndex,
		SummaryIndex:   0,
		Text:           full,
		SequenceNumber: p.nextSeq(),
	}
	if err := p.marshalSend(adapteropenai.ResponsesEventReasoningSummaryDone, done); err != nil {
		return err
	}
	item := adapteropenai.ResponsesOutputItem{
		Type: "reasoning", ID: p.reasoningItemID, Status: "", Role: "",
		Content: nil, Summary: []adapteropenai.ResponsesSummaryPart{{Type: "summary_text", Text: full}}, CallID: "", Name: "", Arguments: "",
	}
	return p.emitOutputItem(adapteropenai.ResponsesEventOutputItemDone, p.reasoningOutputIndex, item)
}

func (p *responsesStreamWriter) handleText(text string) error {
	if text == "" {
		return nil
	}
	if err := p.openMessage(); err != nil {
		return err
	}
	p.messageText.WriteString(text)
	evt := adapteropenai.ResponsesOutputTextDeltaEvent{
		Type:           adapteropenai.ResponsesEventOutputTextDelta,
		ItemID:         p.messageItemID,
		OutputIndex:    p.messageOutputIndex,
		ContentIndex:   0,
		Delta:          text,
		SequenceNumber: p.nextSeq(),
	}
	return p.marshalSend(adapteropenai.ResponsesEventOutputTextDelta, evt)
}

func (p *responsesStreamWriter) openMessage() error {
	if p.messageOpen {
		return nil
	}
	if err := p.closeReasoning(); err != nil {
		return err
	}
	p.messageOpen = true
	p.messageItemID = "msg_" + p.itemBase
	p.messageOutputIndex = p.nextOutputIndex
	p.nextOutputIndex++
	skeleton := adapteropenai.ResponsesOutputItem{
		Type: "message", ID: p.messageItemID, Status: "in_progress", Role: "assistant",
		Content: []adapteropenai.ResponsesContentPart{}, Summary: nil, CallID: "", Name: "", Arguments: "",
	}
	if err := p.emitOutputItem(adapteropenai.ResponsesEventOutputItemAdded, p.messageOutputIndex, skeleton); err != nil {
		return err
	}
	part := adapteropenai.ResponsesContentPart{Type: "output_text", Text: "", Annotations: []adapteropenai.ResponsesAnnotation{}}
	evt := adapteropenai.ResponsesContentPartEvent{
		Type:           adapteropenai.ResponsesEventContentPartAdded,
		ItemID:         p.messageItemID,
		OutputIndex:    p.messageOutputIndex,
		ContentIndex:   0,
		Part:           part,
		SequenceNumber: p.nextSeq(),
	}
	return p.marshalSend(adapteropenai.ResponsesEventContentPartAdded, evt)
}

func (p *responsesStreamWriter) closeMessage() error {
	if !p.messageOpen {
		return nil
	}
	p.messageOpen = false
	full := p.messageText.String()
	textDone := adapteropenai.ResponsesOutputTextDoneEvent{
		Type:           adapteropenai.ResponsesEventOutputTextDone,
		ItemID:         p.messageItemID,
		OutputIndex:    p.messageOutputIndex,
		ContentIndex:   0,
		Text:           full,
		SequenceNumber: p.nextSeq(),
	}
	if err := p.marshalSend(adapteropenai.ResponsesEventOutputTextDone, textDone); err != nil {
		return err
	}
	part := adapteropenai.ResponsesContentPart{Type: "output_text", Text: full, Annotations: []adapteropenai.ResponsesAnnotation{}}
	partDone := adapteropenai.ResponsesContentPartEvent{
		Type:           adapteropenai.ResponsesEventContentPartDone,
		ItemID:         p.messageItemID,
		OutputIndex:    p.messageOutputIndex,
		ContentIndex:   0,
		Part:           part,
		SequenceNumber: p.nextSeq(),
	}
	if err := p.marshalSend(adapteropenai.ResponsesEventContentPartDone, partDone); err != nil {
		return err
	}
	item := adapteropenai.ResponsesOutputItem{
		Type: "message", ID: p.messageItemID, Status: "completed", Role: "assistant",
		Content: []adapteropenai.ResponsesContentPart{part}, Summary: nil, CallID: "", Name: "", Arguments: "",
	}
	return p.emitOutputItem(adapteropenai.ResponsesEventOutputItemDone, p.messageOutputIndex, item)
}

func (p *responsesStreamWriter) handleToolCalls(toolCalls []adapteropenai.ToolCall) error {
	if err := p.closeReasoning(); err != nil {
		return err
	}
	if err := p.closeMessage(); err != nil {
		return err
	}
	for _, tc := range toolCalls {
		state, isNew := p.getOrCreateTool(tc)
		if isNew {
			item := adapteropenai.ResponsesOutputItem{
				Type: "function_call", ID: state.itemID, Status: "in_progress", Role: "",
				Content: nil, Summary: nil, CallID: state.callID, Name: state.name, Arguments: "",
			}
			if err := p.emitOutputItem(adapteropenai.ResponsesEventOutputItemAdded, state.outputIndex, item); err != nil {
				return err
			}
		} else {
			if tc.Function.Name != "" {
				state.name = tc.Function.Name
			}
			if tc.ID != "" {
				state.callID = tc.ID
			}
		}
		if tc.Function.Arguments != "" {
			state.args.WriteString(tc.Function.Arguments)
			evt := adapteropenai.ResponsesFunctionArgsDeltaEvent{
				Type:           adapteropenai.ResponsesEventFunctionArgsDelta,
				ItemID:         state.itemID,
				OutputIndex:    state.outputIndex,
				Delta:          tc.Function.Arguments,
				SequenceNumber: p.nextSeq(),
			}
			if err := p.marshalSend(adapteropenai.ResponsesEventFunctionArgsDelta, evt); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *responsesStreamWriter) getOrCreateTool(tc adapteropenai.ToolCall) (*responsesStreamToolState, bool) {
	if state, ok := p.toolStates[tc.Index]; ok {
		return state, false
	}
	state := &responsesStreamToolState{
		itemID:      "fc_" + p.itemBase + "_" + strconv.Itoa(tc.Index),
		callID:      responsesStreamCallID(p.itemBase, tc),
		name:        tc.Function.Name,
		outputIndex: p.nextOutputIndex,
		index:       tc.Index,
		args:        strings.Builder{},
	}
	p.nextOutputIndex++
	p.toolStates[tc.Index] = state
	p.toolOrder = append(p.toolOrder, tc.Index)
	return state, true
}

func responsesStreamCallID(base string, tc adapteropenai.ToolCall) string {
	if tc.ID != "" {
		return tc.ID
	}
	return "call_" + base + "_" + strconv.Itoa(tc.Index)
}

func (p *responsesStreamWriter) closeTools() error {
	for _, idx := range p.toolOrder {
		state := p.toolStates[idx]
		args := state.args.String()
		done := adapteropenai.ResponsesFunctionArgsDoneEvent{
			Type:           adapteropenai.ResponsesEventFunctionArgsDone,
			ItemID:         state.itemID,
			OutputIndex:    state.outputIndex,
			Arguments:      args,
			SequenceNumber: p.nextSeq(),
		}
		if err := p.marshalSend(adapteropenai.ResponsesEventFunctionArgsDone, done); err != nil {
			return err
		}
		item := adapteropenai.ResponsesOutputItem{
			Type: "function_call", ID: state.itemID, Status: "completed", Role: "",
			Content: nil, Summary: nil, CallID: state.callID, Name: state.name, Arguments: args,
		}
		if err := p.emitOutputItem(adapteropenai.ResponsesEventOutputItemDone, state.outputIndex, item); err != nil {
			return err
		}
	}
	return nil
}

func (p *responsesStreamWriter) collectedToolCalls() []adapteropenai.ToolCall {
	if len(p.toolOrder) == 0 {
		return nil
	}
	out := make([]adapteropenai.ToolCall, 0, len(p.toolOrder))
	for _, idx := range p.toolOrder {
		state := p.toolStates[idx]
		out = append(out, adapteropenai.ToolCall{
			Index:    state.index,
			ID:       state.callID,
			Type:     "function",
			Function: adapteropenai.ToolCallFunction{Name: state.name, Arguments: state.args.String()},
		})
	}
	return out
}

func (p *responsesStreamWriter) buildResponse(status adapteropenai.ResponsesStatus, usage *adapteropenai.Usage) adapteropenai.ResponsesResponse {
	return adapteropenai.BuildResponsesResponse(adapteropenai.ResponsesResponseParams{
		ID:         p.responseID,
		Model:      p.model,
		CreatedAt:  p.createdAt,
		Status:     status,
		Text:       p.messageText.String(),
		Reasoning:  p.reasoningText.String(),
		Refusal:    "",
		ToolCalls:  p.collectedToolCalls(),
		Usage:      usage,
		ItemIDBase: p.itemBase,
	})
}

func (p *responsesStreamWriter) emitEnvelope(name string, resp adapteropenai.ResponsesResponse) error {
	evt := adapteropenai.ResponsesEnvelopeEvent{Type: name, Response: resp, SequenceNumber: p.nextSeq()}
	return p.marshalSend(name, evt)
}

func (p *responsesStreamWriter) emitOutputItem(name string, outputIndex int, item adapteropenai.ResponsesOutputItem) error {
	evt := adapteropenai.ResponsesOutputItemEvent{Type: name, OutputIndex: outputIndex, Item: item, SequenceNumber: p.nextSeq()}
	return p.marshalSend(name, evt)
}

// marshalSend serializes one typed Responses event and writes it as a
// named SSE frame.
func (p *responsesStreamWriter) marshalSend(name string, evt adapteropenai.ResponsesStreamEvent) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		if p.log != nil {
			p.log.Warn("adapter.responses.marshal_failed", "concern", "adapter.chat.render", "event", name, "err", err)
		}
		return fmt.Errorf("marshal responses event %q: %w", name, err)
	}
	return p.send(name, payload)
}

var _ adapterprovider.EventWriter = (*responsesStreamWriter)(nil)
