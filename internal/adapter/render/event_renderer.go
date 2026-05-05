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

// Event is the provider-neutral render input. It deliberately models only the
// surfaces that may produce BYOK-visible output or OpenAI-shaped stream fields.
//
// EncryptedContent is the opaque server-signed `encrypted_content` blob the
// codex parser scrapes off `response.output_item.done` reasoning items and
// surfaces on [EventReasoningFinished]. The renderer embeds it inline on
// the synthetic-thinking close marker as a `data-encrypted` attribute so
// Cursor's transcript owns persistence across turns. Empty for providers
// that do not surface an encrypted_content blob (Anthropic, legacy spans).
type Event struct {
	Kind             EventKind
	Text             string
	ReasoningKind    string
	SummaryIndex     *int
	ToolCalls        []adapteropenai.ToolCall
	ItemID           string
	ItemType         string
	EncryptedContent string
}

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
	// captured from EventReasoningSignaled or EventReasoningDelta. Used as
	// the data-ref attribute on the synthetic-thinking open marker so a
	// later turn can correlate the round-tripped envelope with provider
	// state (e.g. a stored Codex encrypted_content blob). Empty when the
	// upstream did not surface an id, in which case the open marker is
	// emitted attribute-less.
	lastReasoningItemID string
	// lastReasoningEncrypted is the most recent encrypted_content blob
	// captured from a reasoning event (today: codex
	// EventReasoningFinished). The synthetic-thinking close marker
	// embeds it as `data-encrypted` so Cursor's transcript carries it
	// across turns. Cleared after each close so a later span starts
	// fresh.
	lastReasoningEncrypted string
	assistantText          assistantTextAggregate
	assistantTextLogged    bool
	toolCallNames          []string
	hasSubagentToolCall    bool
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
		createdUnix:            renderClock.Now().Unix(),
		modelAlias:             modelAlias,
		reqID:                  reqID,
		backend:                backend,
		ctx:                    ctx,
		log:                    log,
		suppressed:             nil,
		seenRole:               false,
		reasoningOpen:          false,
		lastReasoningKind:      "",
		lastSummaryIdx:         0,
		haveSummaryIdx:         false,
		pendingReasoningBreak:  false,
		reasoningSignaled:      false,
		reasoningVisible:       false,
		reasoningBodyEmitted:   false,
		lastReasoningItemID:    "",
		lastReasoningEncrypted: "",
		upstreamResponseID:     "",
		assistantText:          assistantTextAggregate{deltaCount: 0, chars: 0, text: strings.Builder{}},
		assistantTextLogged:    false,
		toolCallNames:          nil,
		hasSubagentToolCall:    false,
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
	if ev.Kind == EventToolCallDelta {
		r.recordToolCallNames(ev.ToolCalls)
	}
	logEvent := shouldLogEvent(ev)
	if logEvent {
		r.flushSuppressedEventSummaries(r.logContext())
		r.logNormalized(ev)
	} else {
		r.recordSuppressedEvent(ev)
	}
	var out []adapteropenai.StreamChunk
	switch ev.Kind {
	case EventReasoningSignaled:
		r.reasoningSignaled = true
		r.captureReasoningItemID(ev)
		// Open the synthetic content block that makes reasoning visible in Cursor BYOK.
		// Later reasoning deltas fill it; otherwise finish closes an empty block.
		if !r.reasoningVisible && !r.reasoningOpen {
			if chunk := r.renderReasoningOpen(); chunk != nil {
				r.reasoningVisible = true
				out = append(out, *chunk)
			}
		}
	case EventReasoningDelta:
		r.reasoningSignaled = true
		r.reasoningVisible = true
		// Fill the synthetic content block and also populate reasoning_content.
		if chunk := r.renderReasoning(ev); chunk != nil {
			out = append(out, *chunk)
		}
	case EventReasoningFinished:
		// Capture the encrypted_content blob (codex only) so the
		// close marker can carry it inline as `data-encrypted`.
		if enc := strings.TrimSpace(ev.EncryptedContent); enc != "" {
			r.lastReasoningEncrypted = enc
		}
		// Close the synthetic content block; reasoning_content has no marker to close.
		if chunk := r.renderReasoningClose(); chunk != nil {
			out = append(out, *chunk)
		}
	case EventAssistantTextDelta:
		if chunk := r.renderReasoningClose(); chunk != nil {
			out = append(out, *chunk)
		}
		if chunk := r.renderText(ev.Text); chunk != nil {
			out = append(out, *chunk)
		}
	case EventAssistantRefusalDelta:
		if chunk := r.renderReasoningClose(); chunk != nil {
			out = append(out, *chunk)
		}
		if chunk := r.renderRefusal(ev.Text); chunk != nil {
			out = append(out, *chunk)
		}
	case EventToolCallDelta:
		if chunk := r.renderReasoningClose(); chunk != nil {
			out = append(out, *chunk)
		}
		if chunk := r.renderToolCalls(ev.ToolCalls); chunk != nil {
			out = append(out, *chunk)
		}
	}
	for _, ch := range out {
		if logEvent {
			r.logRender(ev, ch)
		}
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
