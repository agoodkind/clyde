package adapter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/clock"
)

// responsesStreamWriter implements provider.EventWriter for the OpenAI
// Responses streaming path. It consumes the same normalized render
// events the chat writer consumes, but translates them into the
// Responses named-event SSE sequence in normalized event order instead of
// chat completion chunks.
// It never emits a `data: [DONE]` terminator; the terminal frame is
// response.completed, response.incomplete, or response.failed.
type responsesStreamWriter struct {
	sse        *adapteropenai.SSEWriter
	flusher    http.Flusher
	log        *slog.Logger
	responseID string
	model      string
	itemBase   string
	createdAt  int64
	warnings   []adaptercompat.CompatibilityWarning

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
	messageParts       []responsesStreamContentState

	toolStates map[int]*responsesStreamToolState
	toolOrder  []int
}

type responsesStreamContentState struct {
	kind string
	text strings.Builder
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
	completed   bool
}

// newResponsesStreamWriter builds a Responses streaming writer over the
// response writer. The responseID is the stable resp_ id allocated at
// handler entry; item ids derive from it so the streamed events and the
// terminal object share ids.
func newResponsesStreamWriter(w http.ResponseWriter, responseID, model string, warnings []adaptercompat.CompatibilityWarning, log *slog.Logger) (*responsesStreamWriter, error) {
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
		warnings:             warnings,
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
		messageParts:         nil,
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
	// Warnings appear only on response.created. Every later event avoids
	// repeating the same compatibility metadata.
	if len(p.warnings) > 0 {
		inProgress.Clyde = &adapteropenai.ResponsesClyde{Warnings: p.warnings}
	}
	if err := p.emitEnvelope(adapteropenai.ResponsesEventCreated, inProgress); err != nil {
		return err
	}
	inProgress.Clyde = nil
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
		return p.handleRefusal(e.Text)
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

// finish closes any open reasoning, message, and function-call items, then
// emits the terminal response.completed or response.incomplete event.
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
	status, incompleteDetails := adapteropenai.ResponsesTerminalForFinishReason(result.FinishReason)
	usage := result.Usage
	final := p.buildResponse(status, &usage)
	final.IncompleteDetails = incompleteDetails
	eventName := adapteropenai.ResponsesEventCompleted
	if status == adapteropenai.ResponsesStatusIncomplete {
		eventName = adapteropenai.ResponsesEventIncomplete
	}
	return p.emitEnvelope(eventName, final)
}

// fail emits response.failed with the mapped, client-safe error semantics.
func (p *responsesStreamWriter) fail(err error) error {
	if beginErr := p.begin(); beginErr != nil {
		return beginErr
	}
	aerr := adapterErrorFrom(err)
	resp := p.buildResponse(adapteropenai.ResponsesStatusFailed, nil)
	message := aerr.Message
	if !aerr.SafeForClient {
		message = "adapter internal error"
	}
	resp.Error = &adapteropenai.ResponsesError{Code: aerr.Code, Message: message}
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
	contentIndex, err := p.openMessageContentPart("output_text")
	if err != nil {
		return err
	}
	p.messageParts[contentIndex].text.WriteString(text)
	evt := adapteropenai.ResponsesOutputTextDeltaEvent{
		Type:           adapteropenai.ResponsesEventOutputTextDelta,
		ItemID:         p.messageItemID,
		OutputIndex:    p.messageOutputIndex,
		ContentIndex:   contentIndex,
		Delta:          text,
		SequenceNumber: p.nextSeq(),
	}
	return p.marshalSend(adapteropenai.ResponsesEventOutputTextDelta, evt)
}

func (p *responsesStreamWriter) handleRefusal(text string) error {
	if text == "" {
		return nil
	}
	if err := p.openMessage(); err != nil {
		return err
	}
	contentIndex, err := p.openMessageContentPart("refusal")
	if err != nil {
		return err
	}
	p.messageParts[contentIndex].text.WriteString(text)
	evt := adapteropenai.ResponsesRefusalDeltaEvent{
		Type:           adapteropenai.ResponsesEventRefusalDelta,
		ItemID:         p.messageItemID,
		OutputIndex:    p.messageOutputIndex,
		ContentIndex:   contentIndex,
		Delta:          text,
		SequenceNumber: p.nextSeq(),
	}
	return p.marshalSend(adapteropenai.ResponsesEventRefusalDelta, evt)
}

func (p *responsesStreamWriter) openMessage() error {
	if p.messageOpen {
		return nil
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
	return nil
}

func (p *responsesStreamWriter) openMessageContentPart(kind string) (int, error) {
	count := len(p.messageParts)
	if count > 0 && p.messageParts[count-1].kind == kind {
		return count - 1, nil
	}
	p.messageParts = append(p.messageParts, responsesStreamContentState{kind: kind, text: strings.Builder{}})
	contentIndex := len(p.messageParts) - 1
	part := responsesContentPart(kind, "")
	evt := adapteropenai.ResponsesContentPartEvent{
		Type:           adapteropenai.ResponsesEventContentPartAdded,
		ItemID:         p.messageItemID,
		OutputIndex:    p.messageOutputIndex,
		ContentIndex:   contentIndex,
		Part:           part,
		SequenceNumber: p.nextSeq(),
	}
	if err := p.marshalSend(adapteropenai.ResponsesEventContentPartAdded, evt); err != nil {
		return 0, err
	}
	return contentIndex, nil
}

func (p *responsesStreamWriter) closeMessage() error {
	if !p.messageOpen {
		return nil
	}
	p.messageOpen = false
	content := make([]adapteropenai.ResponsesContentPart, 0, len(p.messageParts))
	for contentIndex, state := range p.messageParts {
		full := state.text.String()
		part := responsesContentPart(state.kind, full)
		if state.kind == "output_text" {
			textDone := adapteropenai.ResponsesOutputTextDoneEvent{
				Type: adapteropenai.ResponsesEventOutputTextDone, ItemID: p.messageItemID,
				OutputIndex: p.messageOutputIndex, ContentIndex: contentIndex, Text: full, SequenceNumber: p.nextSeq(),
			}
			if err := p.marshalSend(adapteropenai.ResponsesEventOutputTextDone, textDone); err != nil {
				return err
			}
		} else {
			refusalDone := adapteropenai.ResponsesRefusalDoneEvent{
				Type: adapteropenai.ResponsesEventRefusalDone, ItemID: p.messageItemID,
				OutputIndex: p.messageOutputIndex, ContentIndex: contentIndex, Refusal: full, SequenceNumber: p.nextSeq(),
			}
			if err := p.marshalSend(adapteropenai.ResponsesEventRefusalDone, refusalDone); err != nil {
				return err
			}
		}
		partDone := adapteropenai.ResponsesContentPartEvent{
			Type: adapteropenai.ResponsesEventContentPartDone, ItemID: p.messageItemID,
			OutputIndex: p.messageOutputIndex, ContentIndex: contentIndex, Part: part, SequenceNumber: p.nextSeq(),
		}
		if err := p.marshalSend(adapteropenai.ResponsesEventContentPartDone, partDone); err != nil {
			return err
		}
		content = append(content, part)
	}
	item := adapteropenai.ResponsesOutputItem{
		Type: "message", ID: p.messageItemID, Status: "completed", Role: "assistant",
		Content: content, Summary: nil, CallID: "", Name: "", Arguments: "",
	}
	return p.emitOutputItem(adapteropenai.ResponsesEventOutputItemDone, p.messageOutputIndex, item)
}

func (p *responsesStreamWriter) handleToolCalls(toolCalls []adapteropenai.ToolCall) error {
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
		}
		if !isNew {
			if tc.Function.Name != "" {
				state.name = tc.Function.Name
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
		completed:   false,
	}
	p.nextOutputIndex++
	p.toolStates[tc.Index] = state
	p.toolOrder = append(p.toolOrder, tc.Index)
	return state, true
}

func responsesStreamCallID(base string, tc adapteropenai.ToolCall) string {
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
			Name:           state.name,
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
		state.completed = true
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
		Text:       "",
		Reasoning:  "",
		Refusal:    "",
		ToolCalls:  p.collectedToolCalls(),
		Output:     p.orderedOutput(),
		Usage:      usage,
		ItemIDBase: p.itemBase,
		// Warnings ride on the response object only through begin(), which
		// sets Clyde on the first snapshot; buildResponse leaves it unset so
		// terminal frames stay warning-free.
		Warnings: nil,
	})
}

func responsesContentPart(kind, text string) adapteropenai.ResponsesContentPart {
	if kind == "refusal" {
		return adapteropenai.ResponsesContentPart{Type: "refusal", Text: "", Refusal: text, Annotations: nil}
	}
	return adapteropenai.ResponsesContentPart{Type: "output_text", Text: text, Refusal: "", Annotations: []adapteropenai.ResponsesAnnotation{}}
}

func (p *responsesStreamWriter) orderedOutput() []adapteropenai.ResponsesOutputItem {
	if p.nextOutputIndex == 0 {
		return []adapteropenai.ResponsesOutputItem{}
	}
	output := make([]adapteropenai.ResponsesOutputItem, p.nextOutputIndex)
	if p.messageItemID != "" {
		content := make([]adapteropenai.ResponsesContentPart, 0, len(p.messageParts))
		for _, state := range p.messageParts {
			content = append(content, responsesContentPart(state.kind, state.text.String()))
		}
		status := "in_progress"
		if !p.messageOpen {
			status = "completed"
		}
		output[p.messageOutputIndex] = adapteropenai.ResponsesOutputItem{
			Type: "message", ID: p.messageItemID, Status: status, Role: "assistant", Content: content,
			Summary: nil, CallID: "", Name: "", Arguments: "",
		}
	}
	if p.reasoningItemID != "" {
		summary := []adapteropenai.ResponsesSummaryPart{{Type: "summary_text", Text: p.reasoningText.String()}}
		output[p.reasoningOutputIndex] = adapteropenai.ResponsesOutputItem{
			Type: "reasoning", ID: p.reasoningItemID, Status: "", Role: "", Content: nil,
			Summary: summary, CallID: "", Name: "", Arguments: "",
		}
	}
	for _, index := range p.toolOrder {
		state := p.toolStates[index]
		status := "in_progress"
		if state.completed {
			status = "completed"
		}
		output[state.outputIndex] = adapteropenai.ResponsesOutputItem{
			Type: "function_call", ID: state.itemID, Status: status, Role: "", Content: nil,
			Summary: nil, CallID: state.callID, Name: state.name, Arguments: state.args.String(),
		}
	}
	return output
}

func responsesOutputFromEvents(responseID string, events []adapterrender.Event) []adapteropenai.ResponsesOutputItem {
	output := make([]adapteropenai.ResponsesOutputItem, 0)
	base := responsesItemBase(responseID)
	messageIndex := -1
	reasoningIndex := -1
	toolIndexes := make(map[int]int)

	for _, event := range events {
		switch typed := event.(type) {
		case adapterrender.TextDelta:
			output, messageIndex = appendCollectedMessagePart(output, messageIndex, base, "output_text", typed.Text)
		case adapterrender.RefusalDelta:
			output, messageIndex = appendCollectedMessagePart(output, messageIndex, base, "refusal", typed.Text)
		case adapterrender.ReasoningDelta:
			if typed.Text == "" {
				continue
			}
			if reasoningIndex < 0 {
				reasoningIndex = len(output)
				output = append(output, adapteropenai.ResponsesOutputItem{
					Type: "reasoning", ID: "rs_" + base, Status: "", Role: "", Content: nil,
					Summary: []adapteropenai.ResponsesSummaryPart{{Type: "summary_text", Text: typed.Text}},
					CallID:  "", Name: "", Arguments: "",
				})
				continue
			}
			output[reasoningIndex].Summary[0].Text += typed.Text
		case adapterrender.ToolCallDelta:
			for _, toolCall := range typed.ToolCalls {
				outputIndex, found := toolIndexes[toolCall.Index]
				if !found {
					outputIndex = len(output)
					toolIndexes[toolCall.Index] = outputIndex
					output = append(output, adapteropenai.ResponsesOutputItem{
						Type: "function_call", ID: "fc_" + base + "_" + strconv.Itoa(toolCall.Index), Status: "completed", Role: "",
						Content: nil, Summary: nil, CallID: "call_" + base + "_" + strconv.Itoa(toolCall.Index),
						Name: toolCall.Function.Name, Arguments: toolCall.Function.Arguments,
					})
					continue
				}
				item := &output[outputIndex]
				if toolCall.Function.Name != "" {
					item.Name = toolCall.Function.Name
				}
				item.Arguments += toolCall.Function.Arguments
			}
		}
	}
	return output
}

func appendCollectedMessagePart(
	output []adapteropenai.ResponsesOutputItem,
	messageIndex int,
	base string,
	kind string,
	text string,
) ([]adapteropenai.ResponsesOutputItem, int) {
	if text == "" {
		return output, messageIndex
	}
	if messageIndex < 0 {
		messageIndex = len(output)
		output = append(output, adapteropenai.ResponsesOutputItem{
			Type: "message", ID: "msg_" + base, Status: "completed", Role: "assistant",
			Content: []adapteropenai.ResponsesContentPart{}, Summary: nil, CallID: "", Name: "", Arguments: "",
		})
	}
	item := &output[messageIndex]
	contentCount := len(item.Content)
	if contentCount > 0 && item.Content[contentCount-1].Type == kind {
		if kind == "refusal" {
			item.Content[contentCount-1].Refusal += text
		} else {
			item.Content[contentCount-1].Text += text
		}
		return output, messageIndex
	}
	item.Content = append(item.Content, responsesContentPart(kind, text))
	return output, messageIndex
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
