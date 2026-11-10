package anthropicbackend

import (
	"context"
	"log/slog"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

type fakeResponseDispatcher struct {
	streamEvents func(context.Context, anthropic.Request, anthropic.EventSink) (anthropic.Usage, string, error)
	sseWriter    *fakeResponseSSEWriter
	renderer     *adapterrender.EventRenderer
	events       []adapterrender.Event
}

func (f *fakeResponseDispatcher) Log() *slog.Logger {
	return slog.Default()
}

func (f *fakeResponseDispatcher) AnthropicStreamClient() StreamClient {
	return fakeStreamClient{streamEvents: f.streamEvents}
}

func (f *fakeResponseDispatcher) TrackAnthropicContextUsage(_ string, usage adapteropenai.Usage) TrackedUsage {
	return TrackedUsage{Usage: usage, RawPrompt: usage.PromptTokens, RawTotal: usage.TotalTokens}
}

func (f *fakeResponseDispatcher) LogTerminal(context.Context, adapterruntime.RequestEvent) {
}

func (f *fakeResponseDispatcher) CacheTTL() string {
	return ""
}

func (f *fakeResponseDispatcher) WriteEvent(ev adapterrender.Event) error {
	f.events = append(f.events, ev)
	if f.sseWriter != nil {
		if f.renderer == nil {
			f.renderer = adapterrender.NewEventRenderer("req-stream-test", "alias", "anthropic", nil)
		}
		f.sseWriter.chunks = append(f.sseWriter.chunks, f.renderer.HandleEvent(ev)...)
	}
	return nil
}

func (f *fakeResponseDispatcher) FlushEventWriter() error {
	return nil
}

type fakeResponseSSEWriter struct {
	chunks []adapteropenai.StreamChunk
}

type fakeStreamClient struct {
	streamEvents func(context.Context, anthropic.Request, anthropic.EventSink) (anthropic.Usage, string, error)
}

func (f fakeStreamClient) StreamEvents(ctx context.Context, req anthropic.Request, sink anthropic.EventSink) (anthropic.Usage, string, error) {
	if f.streamEvents == nil {
		return anthropic.Usage{}, "", nil
	}
	return f.streamEvents(ctx, req, sink)
}
