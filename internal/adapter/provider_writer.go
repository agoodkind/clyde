package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/slogger"
)

const defaultProviderFinishReason = "stop"

// providerStreamWriter implements provider.EventWriter on top of the
// shared OpenAI SSE writer. Providers emit normalized render events;
// the adapter renders them into streamed OpenAI chunks privately.
type providerStreamWriter struct {
	sse               *adapteropenai.SSEWriter
	systemFingerprint string
	headersWritten    bool
	flusher           http.Flusher
	renderer          *adapterrender.EventRenderer
	reqID             string
	modelAlias        string
	ctx               context.Context
	log               *slog.Logger
	server            *Server
	streamChunkSeq    int
}

func newProviderStreamWriter(
	ctx context.Context,
	s *Server,
	w http.ResponseWriter,
	reqID string,
	modelAlias string,
	backend string,
) (*providerStreamWriter, error) {
	sse, err := adapteropenai.NewSSEWriter(w)
	if err != nil {
		return nil, err
	}
	flusher, _ := w.(http.Flusher)
	return &providerStreamWriter{
		sse:               sse,
		systemFingerprint: systemFingerprint,
		flusher:           flusher,
		renderer:          adapterrender.NewEventRendererWithContext(ctx, reqID, modelAlias, backend, s.log),
		reqID:             reqID,
		modelAlias:        modelAlias,
		ctx:               ctx,
		log:               slogger.WithConcern(s.log, slogger.ConcernAdapterHTTPEgress),
		server:            s,
	}, nil
}

func (p *providerStreamWriter) writeRenderedChunk(ctx context.Context, chunk adapteropenai.StreamChunk) error {
	if !p.headersWritten {
		p.sse.WriteSSEHeaders()
		p.headersWritten = true
	}
	if err := p.sse.EmitStreamChunk(p.systemFingerprint, chunk); err != nil {
		return err
	}
	p.logStreamChunkFlushed(ctx, chunk)
	return nil
}

func (p *providerStreamWriter) logStreamChunkFlushed(ctx context.Context, chunk adapteropenai.StreamChunk) {
	if p == nil {
		return
	}
	p.streamChunkSeq++
	p.logStreamFrameFlushed(ctx, streamFlushLogShapeFromChunk(chunk, p.streamChunkSeq, p.reqID, p.modelAlias))
}

func (p *providerStreamWriter) writeStreamDone(ctx context.Context) error {
	if p == nil || p.sse == nil {
		return nil
	}
	if err := p.sse.WriteStreamDone(); err != nil {
		return err
	}
	p.streamChunkSeq++
	p.logStreamFrameFlushed(ctx, streamFlushLogShape{
		RequestID:        p.reqID,
		Model:            p.modelAlias,
		Sequence:         p.streamChunkSeq,
		PayloadKind:      "done",
		StreamDone:       true,
		ToolCallIDs:      []string{},
		ToolCallNames:    []string{},
		FlushedAtRFC3339: adapterClock.Now().Format(time.RFC3339Nano),
	})
	return nil
}

type streamFlushLogShape struct {
	RequestID                    string
	Model                        string
	Sequence                     int
	PayloadKind                  string
	StreamDone                   bool
	ChoiceCount                  int
	DeltaRolePresent             bool
	DeltaContentPresent          bool
	DeltaToolCallsPresent        bool
	DeltaRefusalPresent          bool
	DeltaReasoningContentPresent bool
	DeltaReasoningPresent        bool
	UsagePresent                 bool
	DeltaContentLength           int
	DeltaReasoningLength         int
	ToolCallCount                int
	ToolCallIDs                  []string
	ToolCallNames                []string
	FinishReason                 string
	FlushedAtRFC3339             string
}

func streamFlushLogShapeFromChunk(chunk adapteropenai.StreamChunk, sequence int, fallbackRequestID string, fallbackModel string) streamFlushLogShape {
	requestID := strings.TrimSpace(chunk.ID)
	if requestID == "" {
		requestID = fallbackRequestID
	}
	model := strings.TrimSpace(chunk.Model)
	if model == "" {
		model = fallbackModel
	}
	payloadKind := strings.TrimSpace(chunk.Object)
	if payloadKind == "" {
		payloadKind = "chat.completion.chunk"
	}
	shape := streamFlushLogShape{
		RequestID:        requestID,
		Model:            model,
		Sequence:         sequence,
		PayloadKind:      payloadKind,
		ChoiceCount:      len(chunk.Choices),
		UsagePresent:     chunk.Usage != nil,
		ToolCallIDs:      []string{},
		ToolCallNames:    []string{},
		FlushedAtRFC3339: adapterClock.Now().Format(time.RFC3339Nano),
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Role != "" {
			shape.DeltaRolePresent = true
		}
		if choice.Delta.Content != "" {
			shape.DeltaContentPresent = true
		}
		if choice.Delta.Refusal != "" {
			shape.DeltaRefusalPresent = true
		}
		if choice.Delta.ReasoningContent != "" {
			shape.DeltaReasoningContentPresent = true
		}
		if choice.Delta.Reasoning != "" {
			shape.DeltaReasoningPresent = true
		}
		shape.DeltaContentLength += len(choice.Delta.Content)
		shape.DeltaReasoningLength += len(choice.Delta.Reasoning) + len(choice.Delta.ReasoningContent)
		shape.ToolCallCount += len(choice.Delta.ToolCalls)
		if len(choice.Delta.ToolCalls) > 0 {
			shape.DeltaToolCallsPresent = true
		}
		if shape.FinishReason == "" && choice.FinishReason != nil {
			shape.FinishReason = strings.TrimSpace(*choice.FinishReason)
		}
		for _, toolCall := range choice.Delta.ToolCalls {
			if id := strings.TrimSpace(toolCall.ID); id != "" {
				shape.ToolCallIDs = append(shape.ToolCallIDs, id)
			}
			if name := strings.TrimSpace(toolCall.Function.Name); name != "" {
				shape.ToolCallNames = append(shape.ToolCallNames, name)
			}
		}
	}
	return shape
}

func (p *providerStreamWriter) logStreamFrameFlushed(ctx context.Context, shape streamFlushLogShape) {
	log := p.log
	if log == nil {
		log = slogger.WithConcern(slog.Default(), slogger.ConcernAdapterHTTPEgress)
	}
	log.LogAttrs(ctx, slog.LevelDebug, "adapter.openai.sse.stream_chunk_flushed",
		slog.String("component", "adapter"),
		slog.String("subcomponent", "openai_sse"),
		slog.String("request_id", shape.RequestID),
		slog.String("model", shape.Model),
		slog.Int("sse_chunk_sequence", shape.Sequence),
		slog.String("sse_payload_kind", shape.PayloadKind),
		slog.Bool("stream_done", shape.StreamDone),
		slog.String("sse_flushed_at", shape.FlushedAtRFC3339),
		slog.Int("choice_count", shape.ChoiceCount),
		slog.Bool("delta_role_present", shape.DeltaRolePresent),
		slog.Bool("delta_content_present", shape.DeltaContentPresent),
		slog.Bool("delta_tool_calls_present", shape.DeltaToolCallsPresent),
		slog.Bool("delta_refusal_present", shape.DeltaRefusalPresent),
		slog.Bool("delta_reasoning_content_present", shape.DeltaReasoningContentPresent),
		slog.Bool("delta_reasoning_present", shape.DeltaReasoningPresent),
		slog.Int("delta_content_length", shape.DeltaContentLength),
		slog.Int("delta_reasoning_length", shape.DeltaReasoningLength),
		slog.Int("tool_call_count", shape.ToolCallCount),
		slog.Any("tool_call_ids", shape.ToolCallIDs),
		slog.Any("tool_call_names", shape.ToolCallNames),
		slog.String("finish_reason", shape.FinishReason),
		slog.Bool("usage_present", shape.UsagePresent),
	)
}

func (p *providerStreamWriter) WriteEvent(ev adapterrender.Event) error {
	if p == nil || p.renderer == nil {
		return nil
	}
	chunks := p.renderer.HandleEvent(ev)
	for _, chunk := range chunks {
		if err := p.writeRenderedChunk(p.ctx, chunk); err != nil {
			return err
		}
	}
	if ev.Kind == adapterrender.EventAssistantTextDelta && len(chunks) > 0 {
		p.renderer.RecordAssistantTextDeltaEmitted(ev.Text)
	}
	return nil
}

func (p *providerStreamWriter) Flush() error {
	return p.flush(p.ctx)
}

func (p *providerStreamWriter) flush(ctx context.Context) error {
	if p != nil && p.renderer != nil {
		p.renderer.Flush(ctx)
	}
	if p.flusher != nil {
		p.flusher.Flush()
	}
	return nil
}

func (p *providerStreamWriter) finalizeStream(ctx context.Context, result adapterprovider.Result, includeUsage bool) error {
	if err := p.flush(ctx); err != nil {
		return err
	}
	for _, notice := range result.UsageNotices {
		includeRole := p == nil || p.renderer == nil || !p.renderer.HasEmittedRole()
		if err := p.writeRenderedChunk(ctx, adapterruntime.OpenAINoticeChunk(p.reqID, p.modelAlias, adapterruntime.FormattedNoticeText(notice.Text), includeRole)); err != nil {
			return err
		}
		adapterruntime.LogNoticeEmission(ctx, notice, "stream_finalized")
	}
	finishReason := normalizedProviderFinishReason(result)
	if p != nil && p.renderer != nil {
		p.renderer.SetUpstreamResponseID(ctx, result.UpstreamResponseID)
		usage := result.Usage
		p.renderer.LogAssistantTextSummary(ctx, finishReason, &usage)
	}
	finishChunk := adapteropenai.StreamChunk{
		ID:      p.reqID,
		Object:  "chat.completion.chunk",
		Created: p.createdUnix(),
		Model:   p.modelAlias,
		Choices: []adapteropenai.StreamChoice{{
			Index:        0,
			Delta:        adapteropenai.StreamDelta{},
			FinishReason: &finishReason,
		}},
	}
	if includeUsage {
		usage := result.Usage
		finishChunk.Usage = &usage
	}
	if err := p.writeRenderedChunk(ctx, finishChunk); err != nil {
		return err
	}
	if includeUsage {
		usage := result.Usage
		if err := p.writeRenderedChunk(ctx, adapteropenai.StreamChunk{
			ID:      p.reqID,
			Object:  "chat.completion.chunk",
			Created: p.createdUnix(),
			Model:   p.modelAlias,
			Choices: []adapteropenai.StreamChoice{},
			Usage:   &usage,
		}); err != nil {
			return err
		}
	}
	return p.writeStreamDone(ctx)
}

func (p *providerStreamWriter) writeStreamErrorBody(ctx context.Context, body adapteropenai.ErrorBody) error {
	if p == nil || p.sse == nil || p.server == nil {
		return nil
	}
	// Route through the boundary helper so every mid-stream error
	// emits exactly one Cursor-safe SSE error frame followed by the
	// [DONE] terminator. The boundary lockdown forbids direct
	// EmitStreamError calls outside adapter_error.go.
	aerr := newAdapterError(adapterErrorUpstreamFailed, body.Message)
	aerr.OpenAIType = body.Type
	aerr.OpenAICode = body.Code
	aerr.OpenAIParam = body.Param
	if err := p.server.respondAdapterStreamError(ctx, p.sse, aerr); err != nil {
		p.log.LogAttrs(ctx, slog.LevelWarn, "adapter.chat.stream_error_responder_failed",
			slog.String("request_id", p.reqID),
			slog.String("model", p.modelAlias),
			slog.Any("err", err),
		)
		return fmt.Errorf("respond stream error: %w", err)
	}
	return nil
}

var _ adapterprovider.EventWriter = (*providerStreamWriter)(nil)

// providerCollectorWriter implements provider.EventWriter for the
// non-streaming response path. It buffers normalized events in memory;
// provider-specific collect reducers assemble final ChatResponses from
// those events after Execute returns.
type providerCollectorWriter struct {
	events []adapterrender.Event
}

func newProviderCollectorWriter() *providerCollectorWriter {
	return &providerCollectorWriter{}
}

func (p *providerCollectorWriter) appendEvent(ev adapterrender.Event) error {
	p.events = append(p.events, ev)
	return nil
}

func (p *providerCollectorWriter) WriteEvent(ev adapterrender.Event) error {
	if p == nil {
		return nil
	}
	return p.appendEvent(ev)
}

func (p *providerCollectorWriter) Flush() error {
	return nil
}

var _ adapterprovider.EventWriter = (*providerCollectorWriter)(nil)

func (p *providerStreamWriter) createdUnix() int64 {
	if p != nil && p.renderer != nil {
		return p.renderer.CreatedUnix()
	}
	return 0
}

func normalizedProviderFinishReason(result adapterprovider.Result) string {
	finishReason := strings.TrimSpace(result.FinishReason)
	if result.ToolCallCount > 0 && finishReason != "length" && finishReason != "content_filter" {
		return "tool_calls"
	}
	if finishReason == "" {
		return defaultProviderFinishReason
	}
	return finishReason
}
