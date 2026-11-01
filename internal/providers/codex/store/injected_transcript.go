package codexstore

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/clyde/internal/transcript"
)

// unmarkedPartIndex is the value that TranscriptRequest.MarkedIndex uses for
// an item without a marked part.
const unmarkedPartIndex = -1

// UnmarkedTranscript stores the content part index, the byte offset where
// generated text starts in that part, and typed spans with offsets in that
// part. Nil spans mean that evidence proves Clyde generated the text but the
// resolver cannot recover its segment kinds.
type UnmarkedTranscript struct {
	PartIndex int
	Start     int
	Spans     []reorienttag.TranscriptSpan
}

// TranscriptRequest stores one saved assistant item, its rollout time, and its
// marked part index. PriorItems stores the rollout items that preceded the
// related compaction for a marked part and is nil for an unmarked item.
type TranscriptRequest struct {
	RawItem     json.RawMessage
	Timestamp   time.Time
	PriorItems  []json.RawMessage
	MarkedIndex int
}

// UnmarkedTranscriptResolver classifies generated text in a saved item. It
// returns false when evidence does not prove that Clyde generated the text.
type UnmarkedTranscriptResolver func(TranscriptRequest) (UnmarkedTranscript, bool)

// rolloutHistory stores the current compaction window and the window that the
// latest compaction replaced. It discards older windows during a stream.
type rolloutHistory struct {
	window     []json.RawMessage
	lastWindow []json.RawMessage
}

// currentWindow returns the items that the next compaction can replace.
func (h *rolloutHistory) currentWindow() []json.RawMessage {
	if h == nil {
		return nil
	}
	return h.window
}

// lastCompactionWindow returns the items that the latest compaction replaced.
func (h *rolloutHistory) lastCompactionWindow() []json.RawMessage {
	if h == nil {
		return nil
	}
	return h.lastWindow
}

// record appends response items to the current window. For a compacted record,
// it saves the current window as the last window and starts a new window from
// replacement_history.
func (h *rolloutHistory) record(envelope historyLine) {
	if h == nil {
		return
	}
	switch historyEnvelopeType(envelope.Type) {
	case historyEnvelopeResponseItem:
		h.window = append(h.window, envelope.Payload)
	case historyEnvelopeCompacted:
		h.lastWindow = h.window
		h.window = nil
		var payload compactedPayload
		if json.Unmarshal(envelope.Payload, &payload) == nil && payload.ReplacementHistory != nil {
			h.window = slices.Clone(*payload.ReplacementHistory)
		}
	case historyEnvelopeSessionMeta, historyEnvelopeEventMsg, historyEnvelopeTurnContext:
	}
}

const (
	// InjectedTranscriptContentKind marks Clyde's pre-compaction transcript part
	// in the content_item_kinds metadata array at the index of the output_text
	// part that Clyde inserted.
	InjectedTranscriptContentKind = "clyde.pre_compaction_transcript.v2"
	injectedTranscriptRole        = "assistant"
	injectedTranscriptContentType = "output_text"
)

// rawContentItemKinds decodes the internal_chat_message_metadata_passthrough
// content_item_kinds array of one saved Codex message item. A null array entry
// decodes to a nil pointer.
type rawContentItemKinds struct {
	Metadata struct {
		ContentItemKinds []*string `json:"content_item_kinds"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

// InjectedTranscriptIndexes returns the content part indexes with
// InjectedTranscriptContentKind in one raw Codex message item. It returns nil
// when the item does not decode.
func InjectedTranscriptIndexes(rawItem json.RawMessage) []int {
	var item rawContentItemKinds
	if json.Unmarshal(rawItem, &item) != nil {
		return nil
	}
	indexes := make([]int, 0, 1)
	for index, kind := range item.Metadata.ContentItemKinds {
		if kind != nil && *kind == InjectedTranscriptContentKind {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

// annotateInjectedTranscripts annotates output_text parts at marked indexes on
// assistant messages. An unmarked item stays ordinary unless the resolver
// proves that Clyde generated its text.
func annotateInjectedTranscripts(items []transcript.CompactedContextItem, timestamp time.Time, opts HistoryOptions) {
	for _, item := range items {
		if item.Kind != transcript.CompactedContextItemKindMessage ||
			item.Message == nil ||
			item.Message.Role != injectedTranscriptRole {
			continue
		}
		indexes := InjectedTranscriptIndexes(item.Message.Raw)
		for _, index := range indexes {
			if index < len(item.Message.Content) && item.Message.Content[index].Type == injectedTranscriptContentType {
				annotateInjectedTranscript(item.Message, index, timestamp, opts)
			}
		}
		if len(indexes) == 0 && compactedMessageQuotesTranscript(item.Message) {
			annotateUnmarkedTranscript(item.Message, timestamp, opts)
		}
	}
}

func compactedMessageQuotesTranscript(message *transcript.CompactedMessageItem) bool {
	return slices.ContainsFunc(message.Content, func(content transcript.CompactedMessageContentItem) bool {
		return strings.Contains(content.Text, reorienttag.PreCompactionTranscriptOpen)
	})
}

// annotateInjectedTranscript attaches framed spans from the part or spans from
// the resolver. It sets UnresolvedTranscript when both fail because the marker
// proves that Clyde generated the part.
func annotateInjectedTranscript(message *transcript.CompactedMessageItem, index int, timestamp time.Time, opts HistoryOptions) {
	content := &message.Content[index]
	spans := reorienttag.CompleteTranscriptSpans(content.Text)
	if len(spans) > 0 {
		content.TranscriptSpans = spans
		return
	}
	resolved, ok := resolveTranscript(message.Raw, timestamp, opts, opts.history.currentWindow(), index)
	if ok && resolved.PartIndex == index && len(resolved.Spans) > 0 {
		content.TranscriptSpans = resolved.Spans
		return
	}
	content.UnresolvedTranscript = true
}

// annotateUnmarkedTranscript calls the resolver for an unmarked assistant
// item. It sets TranscriptSpans or UnresolvedTranscript only when the resolver
// proves generated text.
func annotateUnmarkedTranscript(message *transcript.CompactedMessageItem, timestamp time.Time, opts HistoryOptions) {
	resolved, ok := resolveTranscript(message.Raw, timestamp, opts, nil, unmarkedPartIndex)
	if !ok || resolved.PartIndex >= len(message.Content) {
		return
	}
	content := &message.Content[resolved.PartIndex]
	if len(resolved.Spans) == 0 {
		content.UnresolvedTranscript = true
		return
	}
	content.TranscriptSpans = resolved.Spans
}

func resolveTranscript(raw json.RawMessage, timestamp time.Time, opts HistoryOptions, window []json.RawMessage, markedIndex int) (UnmarkedTranscript, bool) {
	if opts.ResolveUnmarked == nil {
		return UnmarkedTranscript{PartIndex: 0, Start: 0, Spans: nil}, false
	}
	return opts.ResolveUnmarked(TranscriptRequest{
		RawItem:     raw,
		Timestamp:   timestamp,
		PriorItems:  window,
		MarkedIndex: markedIndex,
	})
}

// transcriptTextParts returns ordered text parts for an assistant item with a
// marked part or resolver proof. It returns nil for every other item.
func transcriptTextParts(raw json.RawMessage, payload responsePayload, timestamp time.Time, opts HistoryOptions) []transcript.MessageTextPart {
	if payload.Role != injectedTranscriptRole {
		return nil
	}
	indexes := InjectedTranscriptIndexes(raw)
	if len(indexes) > 0 {
		return markedTextParts(raw, payload, indexes, timestamp, opts)
	}
	if !strings.Contains(concatenatedOutputText(payload.Content), reorienttag.PreCompactionTranscriptOpen) {
		return nil
	}
	resolved, ok := resolveTranscript(raw, timestamp, opts, nil, unmarkedPartIndex)
	if !ok || resolved.PartIndex >= len(payload.Content) {
		return nil
	}
	return recoveredTextParts(payload.Content, resolved)
}

// markedTextParts returns the ordered output_text parts and flags each part at
// a marked index. Each marked part stores spans from its frames or the
// resolver.
func markedTextParts(raw json.RawMessage, payload responsePayload, indexes []int, timestamp time.Time, opts HistoryOptions) []transcript.MessageTextPart {
	parts := make([]transcript.MessageTextPart, 0, len(payload.Content))
	hasMarkedPart := false
	for index, part := range payload.Content {
		if part.Type != injectedTranscriptContentType {
			continue
		}
		marked := slices.Contains(indexes, index)
		hasMarkedPart = hasMarkedPart || marked
		textPart := transcript.MessageTextPart{Text: part.Text, InjectedTranscript: marked, TranscriptSpans: nil}
		if marked {
			textPart.TranscriptSpans = markedPartSpans(raw, part.Text, index, timestamp, opts)
		}
		parts = append(parts, textPart)
	}
	if !hasMarkedPart {
		return nil
	}
	return parts
}

func markedPartSpans(raw json.RawMessage, text string, index int, timestamp time.Time, opts HistoryOptions) []reorienttag.TranscriptSpan {
	if spans := reorienttag.CompleteTranscriptSpans(text); len(spans) > 0 {
		return spans
	}
	resolved, ok := resolveTranscript(raw, timestamp, opts, opts.history.lastCompactionWindow(), index)
	if !ok || resolved.PartIndex != index {
		return nil
	}
	return resolved.Spans
}

// recoveredTextParts splits the resolved part at the generated text boundary.
// Text before that boundary stays ordinary.
func recoveredTextParts(content []contentPart, resolved UnmarkedTranscript) []transcript.MessageTextPart {
	parts := make([]transcript.MessageTextPart, 0, len(content)+1)
	for index, part := range content {
		if part.Type != injectedTranscriptContentType {
			continue
		}
		if index != resolved.PartIndex {
			parts = append(parts, transcript.MessageTextPart{Text: part.Text, InjectedTranscript: false, TranscriptSpans: nil})
			continue
		}
		if resolved.Start > 0 {
			parts = append(parts, transcript.MessageTextPart{Text: part.Text[:resolved.Start], InjectedTranscript: false, TranscriptSpans: nil})
		}
		parts = append(parts, transcript.MessageTextPart{
			Text:               part.Text[resolved.Start:],
			InjectedTranscript: true,
			TranscriptSpans:    shiftedSpans(resolved.Spans, resolved.Start),
		})
	}
	return parts
}

func shiftedSpans(spans []reorienttag.TranscriptSpan, offset int) []reorienttag.TranscriptSpan {
	shifted := make([]reorienttag.TranscriptSpan, 0, len(spans))
	for _, span := range spans {
		span.Start -= offset
		span.End -= offset
		shifted = append(shifted, span)
	}
	return shifted
}

// heldAgentMessage stores an agent_message event until the next emitted
// message or the end of the stream. Matching response metadata can annotate
// its text parts during that time.
type heldAgentMessage struct {
	message *HistoryMessage
}

// flush clears and yields the pending message and returns the consumer's
// continuation result. It returns true without calling the consumer when a
// message is not pending.
func (h *heldAgentMessage) flush(yield func(HistoryMessage, error) bool) bool {
	if h.message == nil {
		return true
	}
	message := *h.message
	h.message = nil
	return yield(message, nil)
}

// applyMarkedResponseItem attaches injected transcript parts to the pending
// message when their concatenated text matches its text after trimming
// surrounding whitespace.
func (h *heldAgentMessage) applyMarkedResponseItem(envelope historyLine, opts HistoryOptions) {
	if h.message == nil || historyEnvelopeType(envelope.Type) != historyEnvelopeResponseItem {
		return
	}
	var payload responsePayload
	if json.Unmarshal(envelope.Payload, &payload) != nil ||
		responseItemType(payload.Type) != responseItemMessageType {
		return
	}
	parts := transcriptTextParts(envelope.Payload, payload, parseCodexTime(envelope.Timestamp), opts)
	if len(parts) == 0 {
		return
	}
	if strings.TrimSpace(h.message.Text) == strings.TrimSpace(concatenatedOutputText(payload.Content)) {
		h.message.TextParts = parts
	}
}

func concatenatedOutputText(parts []contentPart) string {
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == injectedTranscriptContentType {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

func envelopeIsAgentMessage(envelope historyLine) bool {
	if historyEnvelopeType(envelope.Type) != historyEnvelopeEventMsg {
		return false
	}
	var payload eventPayload
	if json.Unmarshal(envelope.Payload, &payload) != nil {
		return false
	}
	return eventMessageType(payload.Type) == eventMessageTypeAgent
}
