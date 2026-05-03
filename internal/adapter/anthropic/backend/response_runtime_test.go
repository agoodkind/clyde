package anthropicbackend

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

// TestBuildErrorBodyForUpstreamMapsClassToType locks in the mapping
// from *UpstreamError class/status to OpenAI ErrorBody.Type and
// .Code so Cursor sees a structured native error rather than an
// assistant-shaped message.
func TestBuildErrorBodyForUpstreamMapsClassToType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		ue       *anthropic.UpstreamError
		wantType string
		wantCode string
	}{
		{
			name: "real_429_is_rate_limit_error",
			ue: &anthropic.UpstreamError{
				Classification: anthropic.Classify(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}, nil),
				Status:         http.StatusTooManyRequests,
				Message:        "rate limited",
			},
			wantType: "rate_limit_error",
			wantCode: "rate_limit_exceeded",
		},
		{
			name: "503_retryable_is_server_error",
			ue: &anthropic.UpstreamError{
				Classification: anthropic.Classify(&http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}}, nil),
				Status:         http.StatusServiceUnavailable,
				Message:        "unavailable",
			},
			wantType: "server_error",
			wantCode: "upstream_unavailable",
		},
		{
			name: "fatal_400_is_server_error",
			ue: &anthropic.UpstreamError{
				Classification: anthropic.Classify(&http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}}, nil),
				Status:         http.StatusBadRequest,
				Message:        "bad request",
			},
			wantType: "server_error",
			wantCode: "upstream_failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildErrorBodyForUpstream(tc.ue)
			if got.Type != tc.wantType {
				t.Fatalf("Type = %q, want %q", got.Type, tc.wantType)
			}
			if got.Code != tc.wantCode {
				t.Fatalf("Code = %q, want %q", got.Code, tc.wantCode)
			}
			if got.Message == "" {
				t.Fatalf("Message must not be empty")
			}
			if !strings.Contains(got.Message, "anthropic") {
				t.Fatalf("Message must include the anthropic prefix from UpstreamError.Error(); got %q", got.Message)
			}
		})
	}
}

func TestBuildErrorBodyForUpstreamNilSafe(t *testing.T) {
	got := buildErrorBodyForUpstream(nil)
	if got.Type != "server_error" || got.Code != "upstream_failed" {
		t.Fatalf("nil UpstreamError error body = %+v, want server_error/upstream_failed", got)
	}
	if got.Message == "" {
		t.Fatalf("nil UpstreamError must still produce a non-empty message")
	}
}

type fakeResponseSSEWriter struct {
	headersCommitted bool
	chunks           []adapteropenai.StreamChunk
	errors           []adapteropenai.ErrorBody
	doneCount        int
}

func (w *fakeResponseSSEWriter) WriteSSEHeaders() {
	w.headersCommitted = true
}

func (w *fakeResponseSSEWriter) EmitStreamChunk(_ string, chunk adapteropenai.StreamChunk) error {
	w.headersCommitted = true
	w.chunks = append(w.chunks, chunk)
	return nil
}

func (w *fakeResponseSSEWriter) EmitStreamError(body adapteropenai.ErrorBody) error {
	w.headersCommitted = true
	w.errors = append(w.errors, body)
	return nil
}

func (w *fakeResponseSSEWriter) WriteStreamDone() error {
	w.doneCount++
	return nil
}

func (w *fakeResponseSSEWriter) HasCommittedHeaders() bool {
	return w.headersCommitted
}

type fakeResponseDispatcher struct {
	log          *slog.Logger
	sseWriter    *fakeResponseSSEWriter
	renderer     *adapterrender.EventRenderer
	events       []adapterrender.Event
	streamEvents func(context.Context, anthropic.Request, anthropic.EventSink) (anthropic.Usage, string, error)
	actionables  int
}

func (d *fakeResponseDispatcher) Log() *slog.Logger {
	if d.log == nil {
		d.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return d.log
}

func (d *fakeResponseDispatcher) EmitRequestStarted(context.Context, adaptermodel.ResolvedModel, string, string, string, bool) {
}

func (d *fakeResponseDispatcher) EmitRequestStreamOpened(context.Context, adaptermodel.ResolvedModel, string, string, string, bool) {
}

func (d *fakeResponseDispatcher) NewAnthropicSSEWriter(http.ResponseWriter) (ResponseSSEWriter, error) {
	if d.sseWriter == nil {
		d.sseWriter = &fakeResponseSSEWriter{}
	}
	return d.sseWriter, nil
}

func (d *fakeResponseDispatcher) AnthropicStreamClient() StreamClient {
	return d
}

func (d *fakeResponseDispatcher) StreamEvents(ctx context.Context, req anthropic.Request, sink anthropic.EventSink) (anthropic.Usage, string, error) {
	if d.streamEvents != nil {
		return d.streamEvents(ctx, req, sink)
	}
	return anthropic.Usage{}, "", nil
}

func (d *fakeResponseDispatcher) SystemFingerprint() string {
	return "fp-test"
}

func (d *fakeResponseDispatcher) StreamChunkHasVisibleContent(chunk adapteropenai.StreamChunk) bool {
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" || choice.Delta.Role != "" {
			return true
		}
	}
	return false
}

func (d *fakeResponseDispatcher) WriteEvent(ev adapterrender.Event) error {
	d.events = append(d.events, ev)
	if d.sseWriter != nil {
		if d.renderer == nil {
			d.renderer = adapterrender.NewEventRenderer("req-test", "alias-test", "anthropic", nil)
		}
		d.sseWriter.chunks = append(d.sseWriter.chunks, d.renderer.HandleEvent(ev)...)
	}
	return nil
}

func (d *fakeResponseDispatcher) FlushEventWriter() error {
	if d.renderer != nil {
		d.renderer.Flush()
	}
	return nil
}

func (d *fakeResponseDispatcher) CollectedEvents() []adapterrender.Event {
	return d.events
}

func (d *fakeResponseDispatcher) TrackAnthropicContextUsage(string, adapteropenai.Usage) TrackedUsage {
	return TrackedUsage{}
}

func (d *fakeResponseDispatcher) WriteJSON(http.ResponseWriter, int, adapteropenai.ChatResponse) {
}

func (d *fakeResponseDispatcher) WriteErrorJSON(http.ResponseWriter, int, adapteropenai.ErrorResponse) {
}

func (d *fakeResponseDispatcher) LogTerminal(context.Context, adapterruntime.RequestEvent) {
}

func (d *fakeResponseDispatcher) LogCacheUsageAnthropic(context.Context, string, string, string, anthropic.Usage) {
}

func (d *fakeResponseDispatcher) CacheTTL() string {
	return ""
}

func (d *fakeResponseDispatcher) NoticesEnabled() bool {
	return true
}

func (d *fakeResponseDispatcher) NoticeUsageThresholdsUsedPercent() []float64 {
	return []float64{75, 95}
}

func (d *fakeResponseDispatcher) ClaimNotice(string, time.Time) bool {
	return true
}

func (d *fakeResponseDispatcher) UnclaimNotice(string, time.Time) {
}

// TestStreamResponse200OverageRejectedEmitsNoticeNotError locks in the
// exact regression called out in the plan: a successful 200 response
// with warning headers must stay on the success path, emit a non-fatal
// notice chunk, and must not surface either a native error envelope or
// the assistant-shaped actionable failure message.
func TestStreamResponse200OverageRejectedEmitsNoticeNotError(t *testing.T) {
	t.Parallel()

	dispatcher := &fakeResponseDispatcher{}
	req := anthropic.Request{Model: "claude-opus-4-7"}
	dispatcher.streamEvents = func(_ context.Context, req anthropic.Request, sink anthropic.EventSink) (anthropic.Usage, string, error) {
		if req.OnHeaders != nil {
			h := http.Header{}
			h.Set("anthropic-ratelimit-unified-status", "allowed")
			h.Set("anthropic-ratelimit-unified-overage-status", "rejected")
			h.Set("anthropic-ratelimit-unified-overage-disabled-reason", "org_level_disabled_until")
			req.OnHeaders(h)
		}
		if err := sink(anthropic.StreamEvent{
			Kind:       "text",
			Text:       "hello from upstream",
			BlockIndex: 0,
		}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := sink(anthropic.StreamEvent{Kind: "stop", StopReason: "end_turn"}); err != nil {
			return anthropic.Usage{}, "", err
		}
		return anthropic.Usage{InputTokens: 11, OutputTokens: 7}, "end_turn", nil
	}

	err := StreamResponse(
		dispatcher,
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		req,
		adaptermodel.ResolvedModel{Alias: "clyde-opus", Backend: "anthropic", ClaudeModel: "claude-opus-4-7"},
		"req-200",
		time.Now(),
		false,
		false,
		"",
	)
	if err != nil {
		t.Fatalf("StreamResponse returned err: %v", err)
	}
	if dispatcher.actionables != 0 {
		t.Fatalf("successful 200 warning path must not emit actionable failure prose; got %d actionable calls", dispatcher.actionables)
	}
	if dispatcher.sseWriter == nil {
		t.Fatal("expected SSE writer to be created")
	}
	if len(dispatcher.sseWriter.errors) != 0 {
		t.Fatalf("successful 200 warning path must not emit native error envelope; got %d", len(dispatcher.sseWriter.errors))
	}
	if len(dispatcher.sseWriter.chunks) == 0 {
		t.Fatal("expected stream chunks to be emitted")
	}
	encoded, err := json.Marshal(dispatcher.sseWriter.chunks)
	if err != nil {
		t.Fatalf("marshal emitted chunks: %v", err)
	}
	body := string(encoded)
	if !strings.Contains(body, "hello from upstream") {
		t.Fatalf("expected assistant content chunk, got %s", body)
	}
	if strings.Contains(body, "Clyde adapter hit an upstream rate limit") {
		t.Fatalf("successful 200 warning path must not emit false rate-limit prose: %s", body)
	}
}

// TestStreamResponseTransportFailureEmitsNativeErrorEnvelope locks in
// the streaming native-error path: a typed retryable transport failure
// must emit a native OpenAI error frame, not an assistant chunk.
func TestStreamResponseTransportFailureEmitsNativeErrorEnvelope(t *testing.T) {
	t.Parallel()

	dispatcher := &fakeResponseDispatcher{
		streamEvents: func(_ context.Context, _ anthropic.Request, _ anthropic.EventSink) (anthropic.Usage, string, error) {
			return anthropic.Usage{}, "", &anthropic.UpstreamError{
				Classification: anthropic.Classify(nil, io.ErrUnexpectedEOF),
				Message:        "post /v1/messages: unexpected EOF",
				Cause:          io.ErrUnexpectedEOF,
			}
		},
	}

	err := StreamResponse(
		dispatcher,
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		anthropic.Request{Model: "claude-opus-4-7"},
		adaptermodel.ResolvedModel{Alias: "clyde-opus", Backend: "anthropic", ClaudeModel: "claude-opus-4-7"},
		"req-eof",
		time.Now(),
		false,
		false,
		"",
	)
	if err != nil {
		t.Fatalf("StreamResponse returned err: %v", err)
	}
	if dispatcher.actionables != 0 {
		t.Fatalf("typed upstream transport failure must not use actionable assistant prose; got %d actionable calls", dispatcher.actionables)
	}
	if dispatcher.sseWriter == nil {
		t.Fatal("expected SSE writer to be created")
	}
	if len(dispatcher.sseWriter.errors) != 1 {
		t.Fatalf("expected exactly 1 native error envelope, got %d", len(dispatcher.sseWriter.errors))
	}
	got := dispatcher.sseWriter.errors[0]
	if got.Type != "server_error" {
		t.Fatalf("error type = %q, want server_error", got.Type)
	}
	if got.Code != "upstream_unavailable" {
		t.Fatalf("error code = %q, want upstream_unavailable", got.Code)
	}
	if !strings.Contains(got.Message, "unexpected EOF") {
		t.Fatalf("error message = %q, want transport failure detail", got.Message)
	}
}

func TestStreamResponse429EmitsRateLimitNativeErrorEnvelope(t *testing.T) {
	t.Parallel()

	dispatcher := &fakeResponseDispatcher{
		streamEvents: func(_ context.Context, _ anthropic.Request, _ anthropic.EventSink) (anthropic.Usage, string, error) {
			resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
			return anthropic.Usage{}, "", &anthropic.UpstreamError{
				Classification: anthropic.Classify(resp, nil),
				Status:         http.StatusTooManyRequests,
				Message:        "This request would exceed your account's rate limit. Please try again later.",
			}
		},
	}

	err := StreamResponse(
		dispatcher,
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		anthropic.Request{Model: "claude-opus-4-7"},
		adaptermodel.ResolvedModel{Alias: "clyde-opus", Backend: "anthropic", ClaudeModel: "claude-opus-4-7"},
		"req-429",
		time.Now(),
		false,
		false,
		"",
	)
	if err != nil {
		t.Fatalf("StreamResponse returned err: %v", err)
	}
	if dispatcher.actionables != 0 {
		t.Fatalf("typed 429 failure must not use actionable assistant prose; got %d actionable calls", dispatcher.actionables)
	}
	if dispatcher.sseWriter == nil {
		t.Fatal("expected SSE writer to be created")
	}
	if len(dispatcher.sseWriter.errors) != 1 {
		t.Fatalf("expected exactly 1 native error envelope, got %d", len(dispatcher.sseWriter.errors))
	}
	got := dispatcher.sseWriter.errors[0]
	if got.Type != "rate_limit_error" {
		t.Fatalf("error type = %q, want rate_limit_error", got.Type)
	}
	if got.Code != "rate_limit_exceeded" {
		t.Fatalf("error code = %q, want rate_limit_exceeded", got.Code)
	}
	if !strings.Contains(got.Message, "rate limit") {
		t.Fatalf("error message = %q, want upstream rate-limit detail", got.Message)
	}
}
