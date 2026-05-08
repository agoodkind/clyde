package render

import (
	"context"
	"log/slog"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/slogger"
)

// EventKind identifies the normalized stream event that the renderer can turn
// into OpenAI-compatible chat completion chunks.
type EventKind string

const (
	// EventAssistantTextDelta is user-visible assistant answer text.
	EventAssistantTextDelta EventKind = "assistant_text_delta"
	// EventAssistantRefusalDelta is OpenAI-shaped refusal text.
	EventAssistantRefusalDelta EventKind = "assistant_refusal_delta"
	// EventReasoningSignaled means the upstream response reported reasoning even
	// if it did not stream a reasoning body.
	EventReasoningSignaled EventKind = "reasoning_signaled"
	// EventReasoningDelta is reasoning text that must be rendered through the
	// synthetic Cursor-visible path and the reasoning_content field.
	EventReasoningDelta EventKind = "reasoning_delta"
	// EventReasoningFinished closes any synthetic Cursor-visible reasoning block.
	EventReasoningFinished EventKind = "reasoning_finished"
	// EventToolCallDelta is an OpenAI-compatible tool call delta.
	EventToolCallDelta EventKind = "tool_call_delta"
)

// Event is the sealed marker interface for normalized adapter stream events.
// The six concrete variants are TextDelta, RefusalDelta, ReasoningSignaled,
// ReasoningDelta, ReasoningFinished, and ToolCallDelta. Consumers type-switch
// on the concrete type.
type Event interface {
	isEvent()
	// eventKind returns the EventKind discriminator for logging and routing.
	eventKind() EventKind
}

// TextDelta carries a user-visible assistant answer text fragment.
type TextDelta struct {
	// Text is the assistant text fragment.
	Text string
}

func (TextDelta) isEvent()             {}
func (TextDelta) eventKind() EventKind { return EventAssistantTextDelta }

// RefusalDelta carries an OpenAI-shaped refusal text fragment.
type RefusalDelta struct {
	// Text is the refusal text fragment.
	Text string
}

func (RefusalDelta) isEvent()             {}
func (RefusalDelta) eventKind() EventKind { return EventAssistantRefusalDelta }

// ReasoningSignaled means the upstream response reported reasoning even if it
// did not stream a reasoning body.
//
// ReasoningKind qualifies the reasoning kind when known (e.g. "redacted" for
// Anthropic redacted_thinking blocks). Empty means the default thinking kind.
//
// ItemID is the provider-assigned id for the reasoning item (Codex). Empty for
// Anthropic and legacy spans.
//
// ItemType is the provider-assigned item type (Codex: "reasoning"). Empty for
// Anthropic.
type ReasoningSignaled struct {
	ReasoningKind string
	ItemID        string
	ItemType      string
}

func (ReasoningSignaled) isEvent()             {}
func (ReasoningSignaled) eventKind() EventKind { return EventReasoningSignaled }

// ReasoningDelta is reasoning text that must be rendered through the synthetic
// Cursor-visible path and the reasoning_content field.
//
// ReasoningKind qualifies the delta kind. "text" for native thinking text.
// "summary" for Codex summary deltas. "redacted" for Anthropic redacted blocks.
// Empty defaults to "text".
//
// SummaryIndex is the Codex reasoning summary part index. Nil for Anthropic.
//
// Signature is the opaque per-thinking-block signature Anthropic emits via the
// signature_delta SSE event. The renderer embeds it on the close marker as
// data-signature. Empty for Codex.
//
// RedactedData is the opaque base64 payload Anthropic emits on a
// redacted_thinking block. The renderer embeds it on the close marker as
// data-encrypted. Empty for every other reasoning kind.
//
// ItemID is the provider-assigned id for the reasoning item (Codex). Empty for
// Anthropic.
//
// ItemType is the provider-assigned item type (Codex: "reasoning"). Empty for
// Anthropic.
type ReasoningDelta struct {
	Text          string
	ReasoningKind string
	SummaryIndex  *int
	Signature     string
	RedactedData  string
	ItemID        string
	ItemType      string
}

func (ReasoningDelta) isEvent()             {}
func (ReasoningDelta) eventKind() EventKind { return EventReasoningDelta }

// ReasoningFinished closes any synthetic Cursor-visible reasoning block.
//
// ReasoningKind qualifies the kind that is closing. "redacted" picks
// SyntheticRedactedThinking; empty picks SyntheticReasoning.
//
// EncryptedContent is the opaque encrypted_content blob Codex emits. The
// renderer embeds it on the close marker as data-encrypted.
//
// Signature is the Anthropic per-block signature for the block being closed.
// Empty for Codex.
//
// ItemID and ItemType identify the upstream reasoning item (Codex).
type ReasoningFinished struct {
	ReasoningKind    string
	EncryptedContent string
	Signature        string
	ItemID           string
	ItemType         string
}

func (ReasoningFinished) isEvent()             {}
func (ReasoningFinished) eventKind() EventKind { return EventReasoningFinished }

// ToolCallDelta carries an OpenAI-compatible tool call delta.
type ToolCallDelta struct {
	ToolCalls []adapteropenai.ToolCall
}

func (ToolCallDelta) isEvent()             {}
func (ToolCallDelta) eventKind() EventKind { return EventToolCallDelta }

// RendererState exposes reasoning state needed by response collectors that need
// to know whether synthetic reasoning was opened during a stream.
type RendererState struct {
	ReasoningSignaled bool
	ReasoningVisible  bool
}

// EventRenderer converts normalized adapter events into OpenAI-compatible stream
// chunks while preserving Cursor-specific synthetic reasoning state.
type EventRenderer struct {
	createdUnix           int64
	modelAlias            string
	reqID                 string
	backend               string
	ctx                   context.Context
	log                   *slog.Logger
	suppressed            map[EventKind]*deltaSummary
	seenRole              bool
	reasoningOpen         bool
	lastReasoningKind     string
	lastSummaryIdx        int
	haveSummaryIdx        bool
	pendingReasoningBreak bool
	reasoningSignaled     bool
	reasoningVisible      bool
	reasoningBodyEmitted  bool
	// lastReasoningItemID is the most recent upstream reasoning item id
	// captured from ReasoningSignaled or ReasoningDelta. Used as
	// the data-ref attribute on the synthetic-thinking open marker so a
	// later turn can correlate the round-tripped envelope with provider
	// state (e.g. a stored Codex encrypted_content blob). Empty when the
	// upstream did not surface an id, in which case the open marker is
	// emitted attribute-less.
	lastReasoningItemID string
	// lastReasoningEncrypted is the most recent encrypted_content blob
	// captured from a reasoning event (today: codex
	// ReasoningFinished). The synthetic-thinking close marker
	// embeds it as `data-encrypted` so Cursor's transcript carries it
	// across turns. Cleared after each close so a later span starts
	// fresh.
	lastReasoningEncrypted string
	// lastReasoningSignature is the most recent Anthropic signature value
	// captured from a reasoning event (Anthropic surfaces it on
	// ReasoningDelta; the close-time emission picks up whatever the
	// most recent value is). The synthetic-thinking close marker embeds
	// it as `data-signature` so Cursor's transcript carries it across
	// turns. Cleared after each close so a later span starts fresh.
	lastReasoningSignature string
	// lastReasoningRedactedData is the most recent opaque base64 payload
	// captured from a reasoning event whose ReasoningKind is "redacted"
	// (today: Anthropic redacted_thinking via ReasoningDelta). The
	// synthetic-redacted-thinking close marker embeds it as
	// `data-encrypted` so Cursor's transcript carries it across turns.
	// Cleared after each close so a later span starts fresh.
	lastReasoningRedactedData string
	assistantText             assistantTextAggregate
	assistantTextLogged       bool
	toolCallNames             []string
	hasSubagentToolCall       bool
	// upstreamResponseID is the most recent provider-assigned response id,
	// captured via SetUpstreamResponseID. It is merged onto the
	// correlation snapshot built from the per-call ctx so summary logs
	// always carry the most recent upstream identifier.
	upstreamResponseID string
}

// NewEventRenderer constructs a renderer with a background context.
func NewEventRenderer(reqID, modelAlias, backend string, log *slog.Logger) *EventRenderer {
	return NewEventRendererWithContext(context.Background(), reqID, modelAlias, backend, log)
}

// NewEventRendererWithContext constructs a renderer with correlation-aware
// logging context attached to each diagnostic log event.
func NewEventRendererWithContext(ctx context.Context, reqID, modelAlias, backend string, log *slog.Logger) *EventRenderer {
	if log == nil {
		log = slog.Default()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	log = slogger.WithConcern(log, slogger.ConcernAdapterChatRender)
	return &EventRenderer{
		createdUnix:               renderClock.Now().Unix(),
		modelAlias:                modelAlias,
		reqID:                     reqID,
		backend:                   backend,
		ctx:                       ctx,
		log:                       log,
		suppressed:                nil,
		seenRole:                  false,
		reasoningOpen:             false,
		lastReasoningKind:         "",
		lastSummaryIdx:            0,
		haveSummaryIdx:            false,
		pendingReasoningBreak:     false,
		reasoningSignaled:         false,
		reasoningVisible:          false,
		reasoningBodyEmitted:      false,
		lastReasoningItemID:       "",
		lastReasoningEncrypted:    "",
		lastReasoningSignature:    "",
		lastReasoningRedactedData: "",
		upstreamResponseID:        "",
		assistantText:             assistantTextAggregate{deltaCount: 0, chars: 0, text: strings.Builder{}},
		assistantTextLogged:       false,
		toolCallNames:             nil,
		hasSubagentToolCall:       false,
	}
}

// State returns whether this renderer observed or displayed reasoning.
func (r *EventRenderer) State() RendererState {
	return RendererState{ReasoningSignaled: r.reasoningSignaled, ReasoningVisible: r.reasoningVisible}
}

// RequestID returns the stream chunk id used by this renderer.
func (r *EventRenderer) RequestID() string { return r.reqID }

// CreatedUnix returns the stream creation timestamp used by this renderer.
func (r *EventRenderer) CreatedUnix() int64 { return r.createdUnix }

// ModelAlias returns the model name written to stream chunks.
func (r *EventRenderer) ModelAlias() string { return r.modelAlias }

// HasEmittedRole reports whether the stream already sent its assistant role.
func (r *EventRenderer) HasEmittedRole() bool {
	if r == nil {
		return false
	}
	return r.seenRole
}

// SetContext replaces the context used for renderer diagnostic logs.
func (r *EventRenderer) SetContext(ctx context.Context) {
	if r == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.ctx = ctx
}

// HandleEvent renders one normalized event into zero or more stream chunks.
// Only assistant text, refusal text, synthetic reasoning, and tool calls produce
// output; all other work in this path is diagnostic bookkeeping.
func (r *EventRenderer) HandleEvent(ev Event) []adapteropenai.StreamChunk {
	if td, ok := ev.(ToolCallDelta); ok {
		r.recordToolCallNames(td.ToolCalls)
	}
	logEvent := r.handleEventDiagnostics(ev)
	out := r.dispatchEvent(ev)
	if logEvent {
		for _, ch := range out {
			r.logRender(ev, ch)
		}
	}
	return out
}

// handleEventDiagnostics flushes any suppressed-event summaries and logs the
// normalized event when it is loggable, or records it as suppressed otherwise.
// Returns true when the event itself was logged (and therefore each rendered
// chunk should also be logged).
func (r *EventRenderer) handleEventDiagnostics(ev Event) bool {
	logEvent := shouldLogEvent(ev)
	if logEvent {
		r.flushSuppressedEventSummaries(r.logContext())
		r.logNormalized(ev)
		return true
	}
	r.recordSuppressedEvent(ev)
	return false
}

// dispatchEvent routes a normalized event to its kind-specific renderer and
// returns the resulting stream chunks in order.
func (r *EventRenderer) dispatchEvent(ev Event) []adapteropenai.StreamChunk {
	switch e := ev.(type) {
	case ReasoningSignaled:
		return r.handleReasoningSignaled(e)
	case ReasoningDelta:
		return r.handleReasoningDelta(e)
	case ReasoningFinished:
		return r.handleReasoningFinished(e)
	case TextDelta:
		return r.handleAssistantTextDelta(e)
	case RefusalDelta:
		return r.handleAssistantRefusalDelta(e)
	case ToolCallDelta:
		return r.handleToolCallDelta(e)
	}
	return nil
}

// handleReasoningSignaled marks reasoning as observed and, when nothing has
// opened the synthetic content block yet, emits the open marker. A non-empty
// ReasoningKind on the event captures the active reasoning kind so the
// renderer routes the open and close markers to the matching synthetic kind
// (today: "redacted" picks SyntheticRedactedThinking; everything else picks
// SyntheticReasoning).
func (r *EventRenderer) handleReasoningSignaled(ev ReasoningSignaled) []adapteropenai.StreamChunk {
	r.reasoningSignaled = true
	r.captureReasoningItemIDFromString(ev.ItemID)
	if kind := strings.TrimSpace(ev.ReasoningKind); kind != "" {
		r.lastReasoningKind = kind
	}
	// Open the synthetic content block that makes reasoning visible in Cursor BYOK.
	// Later reasoning deltas fill it; otherwise finish closes an empty block.
	if r.reasoningVisible || r.reasoningOpen {
		return nil
	}
	chunk := r.renderReasoningOpen()
	if chunk == nil {
		return nil
	}
	r.reasoningVisible = true
	return []adapteropenai.StreamChunk{*chunk}
}

// handleReasoningDelta fills the synthetic content block and also populates
// reasoning_content for clients that consume it directly. A non-empty
// Signature on the event captures the most recent Anthropic signature for
// the active thinking block; renderReasoningClose later embeds it on the
// close marker as `data-signature`. A non-empty RedactedData on the event
// (Anthropic redacted_thinking; ReasoningKind="redacted") captures the
// opaque base64 payload that renderReasoningClose later embeds on the
// redacted-thinking close marker as `data-encrypted`.
func (r *EventRenderer) handleReasoningDelta(ev ReasoningDelta) []adapteropenai.StreamChunk {
	r.reasoningSignaled = true
	r.reasoningVisible = true
	if sig := strings.TrimSpace(ev.Signature); sig != "" {
		r.lastReasoningSignature = sig
	}
	if data := strings.TrimSpace(ev.RedactedData); data != "" {
		r.lastReasoningRedactedData = data
	}
	chunk := r.renderReasoningFromDelta(ev)
	if chunk == nil {
		return nil
	}
	return []adapteropenai.StreamChunk{*chunk}
}

// handleReasoningFinished closes the synthetic content block and captures any
// provider-specific close-marker payload: the codex encrypted_content blob
// and the Anthropic per-block signature. Both are independently optional;
// each provider populates only the field it owns.
func (r *EventRenderer) handleReasoningFinished(ev ReasoningFinished) []adapteropenai.StreamChunk {
	if enc := strings.TrimSpace(ev.EncryptedContent); enc != "" {
		r.lastReasoningEncrypted = enc
	}
	if sig := strings.TrimSpace(ev.Signature); sig != "" {
		r.lastReasoningSignature = sig
	}
	chunk := r.renderReasoningClose()
	if chunk == nil {
		return nil
	}
	return []adapteropenai.StreamChunk{*chunk}
}

// handleAssistantTextDelta closes any open reasoning block and emits the
// assistant text chunk.
func (r *EventRenderer) handleAssistantTextDelta(ev TextDelta) []adapteropenai.StreamChunk {
	return r.appendReasoningCloseAnd(r.renderText(ev.Text))
}

// handleAssistantRefusalDelta closes any open reasoning block and emits the
// refusal chunk.
func (r *EventRenderer) handleAssistantRefusalDelta(ev RefusalDelta) []adapteropenai.StreamChunk {
	return r.appendReasoningCloseAnd(r.renderRefusal(ev.Text))
}

// handleToolCallDelta closes any open reasoning block and emits the tool call
// chunk.
func (r *EventRenderer) handleToolCallDelta(ev ToolCallDelta) []adapteropenai.StreamChunk {
	return r.appendReasoningCloseAnd(r.renderToolCalls(ev.ToolCalls))
}

// appendReasoningCloseAnd prepends a reasoning-close chunk (when one is
// emitted) ahead of the supplied chunk, returning the ordered slice. Either
// chunk may be nil.
func (r *EventRenderer) appendReasoningCloseAnd(chunk *adapteropenai.StreamChunk) []adapteropenai.StreamChunk {
	var out []adapteropenai.StreamChunk
	if closeChunk := r.renderReasoningClose(); closeChunk != nil {
		out = append(out, *closeChunk)
	}
	if chunk != nil {
		out = append(out, *chunk)
	}
	return out
}

func (r *EventRenderer) renderText(text string) *adapteropenai.StreamChunk {
	if strings.TrimSpace(text) == "" && text == "" {
		return nil
	}
	delta := adapteropenai.StreamDelta{Content: text}
	if !r.seenRole {
		delta.Role = "assistant"
		r.seenRole = true
	}
	ch := r.baseChunk(delta)
	return &ch
}

func (r *EventRenderer) renderRefusal(text string) *adapteropenai.StreamChunk {
	if strings.TrimSpace(text) == "" && text == "" {
		return nil
	}
	delta := adapteropenai.StreamDelta{Refusal: text}
	if !r.seenRole {
		delta.Role = "assistant"
		r.seenRole = true
	}
	ch := r.baseChunk(delta)
	return &ch
}

func (r *EventRenderer) renderToolCalls(toolCalls []adapteropenai.ToolCall) *adapteropenai.StreamChunk {
	if len(toolCalls) == 0 {
		return nil
	}
	delta := adapteropenai.StreamDelta{ToolCalls: toolCalls}
	if !r.seenRole {
		delta.Role = "assistant"
		r.seenRole = true
	}
	ch := r.baseChunk(delta)
	return &ch
}

func (r *EventRenderer) baseChunk(delta adapteropenai.StreamDelta) adapteropenai.StreamChunk {
	return adapteropenai.StreamChunk{
		ID:      r.reqID,
		Object:  "chat.completion.chunk",
		Created: r.createdUnix,
		Model:   r.modelAlias,
		Choices: []adapteropenai.StreamChoice{{Index: 0, Delta: delta}},
	}
}
