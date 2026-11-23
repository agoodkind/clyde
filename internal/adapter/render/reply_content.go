package render

import (
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// ReplyBlock identifies the visible bytes generated for one replayable block.
type ReplyBlock struct {
	Start int
	End   int
	Part  SyntheticPart
}

// ReplyContent records provider events separately from model output.
type ReplyContent struct {
	Blocks    []ReplyBlock
	visible   strings.Builder
	reasoning strings.Builder
	start     int
	calls     map[int]*collectedToolCall
}

// CaptureReply tracks the byte positions of generated Markdown.
//
// The stream writer starts one capture per reply before rendering the first
// event. A notice following a 10-byte answer starts at byte 10. Only the
// recorded interval identifies the warning when the client echoes that text.
//
//	reply := renderer.CaptureReply()
func (r *EventRenderer) CaptureReply() *ReplyContent {
	var reply ReplyContent
	reply.calls = make(map[int]*collectedToolCall)
	r.reply = &reply

	return r.reply
}

// CollectReply renders buffered provider events and records their positions.
//
// Collection closes the final reasoning block after the last event.
//
//	reply := renderer.CollectReply(events)
func (r *EventRenderer) CollectReply(events []Event) *ReplyContent {
	reply := r.CaptureReply()

	for _, event := range events {
		r.dispatchEvent(event)
	}

	r.FinishReply()

	return reply
}

// Text returns the visible reply before its trailing reference.
//
// Stored byte offsets refer to this text.
//
//	visible := reply.Text()
func (reply *ReplyContent) Text() string {
	return reply.visible.String()
}

// ToolCalls returns calls in client order with their rendered arguments.
//
// Each call combines the argument fragments for its stream index.
//
//	calls := reply.ToolCalls()
func (reply *ReplyContent) ToolCalls() []adapteropenai.ToolCall {
	return finalizeCollectedToolCalls(reply.calls, NativePatchRepresentationRaw)
}

// SetCollectedText records the final text after provider response formatting.
//
// A formatter can replace the answer (for example, to satisfy a JSON schema).
// In that case keep the visible thinking before the formatted answer.
//
//	reply.SetCollectedText(answer)
func (reply *ReplyContent) SetCollectedText(text string) {
	original := reply.visible.String()
	var answer strings.Builder
	position := 0

	for _, block := range reply.Blocks {
		answer.WriteString(original[position:block.Start])
		position = block.End
	}

	answer.WriteString(original[position:])
	if text == answer.String() {
		return
	}

	reply.visible.Reset()

	for index := range reply.Blocks {
		block := &reply.Blocks[index]
		visible := original[block.Start:block.End]
		block.Start = reply.visible.Len()
		reply.visible.WriteString(visible)
		block.End = reply.visible.Len()
	}

	reply.visible.WriteString(text)
}

// AppendNotice records a visible warning for removal on replay.
//
// The stream writer sends the returned Markdown to the client.
//
//	text := reply.AppendNotice("Weekly usage is near the limit.")
func (reply *ReplyContent) AppendNotice(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}

	text := "\n\n" + formatSyntheticBody(specFor(SyntheticNotice), body, true) + "\n\n"
	start := reply.visible.Len()
	reply.visible.WriteString(text)
	var part SyntheticPart
	part.Kind = SyntheticNotice
	reply.Blocks = append(
		reply.Blocks,
		ReplyBlock{
			Start: start,
			End:   reply.visible.Len(),
			Part:  part,
		},
	)

	return text
}

// FinishReply closes a final reasoning block before persisting its metadata.
//
// A stream writer sends the returned chunk when a block needs closing.
//
//	chunk := renderer.FinishReply()
func (r *EventRenderer) FinishReply() *adapteropenai.StreamChunk {
	return r.renderReasoningClose()
}

func (reply *ReplyContent) record(delta adapteropenai.StreamDelta) {
	reply.visible.WriteString(delta.Content)
	if len(delta.ToolCalls) != 0 {
		accumulateCollectedToolCalls(
			reply.calls,
			ToolCallDelta{ToolCalls: delta.ToolCalls, NativePatchInput: nil},
		)
	}
}
