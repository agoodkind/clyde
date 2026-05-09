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
func (r *EventRenderer) renderReasoningFromDelta(ev ReasoningDelta) *adapteropenai.StreamChunk {
	r.captureReasoningItemIDFromString(ev.ItemID)
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
	decorated := r.decorateReasoningFromDelta(ev)
	kind := r.activeSyntheticReasoningKind()
	contentOut := FormatSyntheticContentDeltaWithRef(kind, open, r.lastReasoningItemID, decorated)
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
	kind := r.activeSyntheticReasoningKind()
	delta := adapteropenai.StreamDelta{Content: SyntheticContentOpenWithRef(kind, r.lastReasoningItemID)}
	if !r.seenRole {
		delta.Role = "assistant"
		r.seenRole = true
	}
	ch := r.baseChunk(delta)
	return &ch
}

// activeSyntheticReasoningKind returns the synthetic content kind that the
// open and close markers should use for the active reasoning span. The
// translator records the ReasoningKind on EventReasoningSignaled and
// EventReasoningDelta; "redacted" picks SyntheticRedactedThinking, every
// other value (including the empty default) picks SyntheticReasoning so the
// existing code path is unchanged.
func (r *EventRenderer) activeSyntheticReasoningKind() SyntheticContentKind {
	if r.lastReasoningKind == "redacted" {
		return SyntheticRedactedThinking
	}
	return SyntheticReasoning
}

// captureReasoningItemIDFromString stores the most recent upstream reasoning
// item id so the open marker can carry it as a data-ref attribute. Codex
// events populate itemID with values like "rs_abc123"; Anthropic events pass
// empty so the renderer emits the legacy attribute-less marker.
func (r *EventRenderer) captureReasoningItemIDFromString(itemID string) {
	if id := strings.TrimSpace(itemID); id != "" {
		r.lastReasoningItemID = id
	}
}

func (r *EventRenderer) decorateReasoningFromDelta(ev ReasoningDelta) string {
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

// renderReasoningClose closes the synthetic Cursor-visible block in
// delta.content. When provider-specific payloads were captured during the
// span (codex encrypted_content via EventReasoningFinished, Anthropic
// signature via EventReasoningDelta or EventReasoningFinished), the close
// marker carries them inline as `data-encrypted` and `data-signature`
// sibling attributes so Cursor's transcript persists them across turns.
// Both captures are cleared after emission so a subsequent reasoning span
// starts fresh.
func (r *EventRenderer) renderReasoningClose() *adapteropenai.StreamChunk {
	if !r.reasoningOpen {
		return nil
	}
	r.reasoningOpen = false
	kind := r.activeSyntheticReasoningKind()
	encrypted := r.lastReasoningEncrypted
	signature := r.lastReasoningSignature
	// For redacted_thinking the data blob rides on `data-encrypted` (the
	// existing close-marker attribute name is reused; the synthetic kind
	// disambiguates the byte semantics). The opaque payload is captured
	// from EventReasoningDelta.RedactedData; the regular thinking
	// encrypted_content path remains untouched.
	if kind == SyntheticRedactedThinking {
		encrypted = r.lastReasoningRedactedData
		signature = ""
	}
	r.lastReasoningEncrypted = ""
	r.lastReasoningSignature = ""
	r.lastReasoningRedactedData = ""
	// Reset the kind so a later reasoning span starts fresh.
	r.lastReasoningKind = ""
	ch := r.baseChunk(adapteropenai.StreamDelta{Content: SyntheticContentCloseWithAttrs(kind, encrypted, signature)})
	return &ch
}
