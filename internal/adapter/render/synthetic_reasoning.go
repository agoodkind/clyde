package render

import (
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// renderReasoning intentionally writes reasoning to two stream surfaces:
//
//   - delta.content gets a marker-wrapped synthetic block from the shared
//     [FormatSyntheticContentDeltaWithRef] fabric because Cursor's custom OpenAI/BYOK
//     ingress does not currently honor reasoning_content the way Cursor honors
//     it for first-party reasoning models. Putting the thinking block in
//     content gives BYOK users the same visible inline-thinking effect.
//   - delta.reasoning_content gets the same reasoning as plain text in case
//     Cursor starts honoring that field again for custom OpenAI/BYOK streams.
//
// Do not remove the delta.content emission just because reasoning_content
// exists. Without the marker-wrapped content path, Cursor BYOK users may see
// no thinking at all. Before the next upstream request, the per-backend
// mapper calls [ExtractSyntheticParts] and materializes each kind per its
// upstream contract (Anthropic emits a native thinking block; Codex drops).
func (r *EventRenderer) renderReasoning(ev Event) *adapteropenai.StreamChunk {
	r.captureReasoningItemID(ev)
	text := strings.TrimSpace(ev.Text)
	if text == "" && ev.Text == "" {
		return nil
	}
	if isThinkingPlaceholder(text) && !r.reasoningBodyEmitted {
		if !r.reasoningOpen {
			return r.renderReasoningOpen()
		}
		return nil
	}
	open := !r.reasoningOpen
	decorated := r.decorateReasoningDelta(ev)
	contentOut := FormatSyntheticContentDeltaWithRef(SyntheticReasoning, open, r.lastReasoningItemID, decorated)
	r.reasoningOpen = true
	r.reasoningBodyEmitted = true
	delta := adapteropenai.StreamDelta{
		Content:          contentOut,
		ReasoningContent: decorated,
	}
	if !r.seenRole {
		delta.Role = "assistant"
		r.seenRole = true
	}
	ch := r.baseChunk(delta)
	return &ch
}

func (r *EventRenderer) renderReasoningOpen() *adapteropenai.StreamChunk {
	r.reasoningOpen = true
	delta := adapteropenai.StreamDelta{Content: SyntheticContentOpenWithRef(SyntheticReasoning, r.lastReasoningItemID)}
	if !r.seenRole {
		delta.Role = "assistant"
		r.seenRole = true
	}
	ch := r.baseChunk(delta)
	return &ch
}

// captureReasoningItemID stores the most recent upstream reasoning item id
// observed on a reasoning event so the open marker can carry it as a
// data-ref attribute. Codex events populate ItemID with values like
// "rs_abc123"; Anthropic events leave it empty so the renderer emits the
// legacy attribute-less marker.
func (r *EventRenderer) captureReasoningItemID(ev Event) {
	if id := strings.TrimSpace(ev.ItemID); id != "" {
		r.lastReasoningItemID = id
	}
}

func (r *EventRenderer) decorateReasoningDelta(ev Event) string {
	prefix := ""
	kind := strings.TrimSpace(ev.ReasoningKind)
	if kind == "" {
		kind = "text"
	}
	if r.pendingReasoningBreak {
		prefix = "\n\n"
		r.pendingReasoningBreak = false
	} else if r.lastReasoningKind != "" && r.lastReasoningKind != kind {
		prefix = "\n\n"
	}
	if kind == "summary" && strings.HasPrefix(strings.TrimSpace(ev.Text), "**") {
		prefix = "\n\n"
	}
	if ev.SummaryIndex != nil {
		if r.haveSummaryIdx && r.lastSummaryIdx != *ev.SummaryIndex {
			prefix = "\n\n"
		}
		r.lastSummaryIdx = *ev.SummaryIndex
		r.haveSummaryIdx = true
	}
	r.lastReasoningKind = kind
	return prefix + ev.Text
}

func isThinkingPlaceholder(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "thinking...", "thinking…":
		return true
	default:
		return false
	}
}

// renderReasoningClose closes the synthetic Cursor-visible block in delta.content.
func (r *EventRenderer) renderReasoningClose() *adapteropenai.StreamChunk {
	if !r.reasoningOpen {
		return nil
	}
	r.reasoningOpen = false
	ch := r.baseChunk(adapteropenai.StreamDelta{Content: SyntheticContentClose(SyntheticReasoning)})
	return &ch
}
