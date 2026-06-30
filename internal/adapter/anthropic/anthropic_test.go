package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/mitm/capture"
)

type staticToken struct{}

func (staticToken) Token(ctx context.Context) (string, error) {
	return "test-token", nil
}

// ForceRefresh satisfies the extended OAuthSource interface. The static fake
// returns the same token it always returns; tests that exercise the retry path
// use the dedicated fakeOAuth helper in client_retry_test.go.
func (staticToken) ForceRefresh(_ context.Context) (string, error) {
	return "test-token", nil
}

type rewriteMessagesHost struct {
	serverURL *url.URL
	inner     http.RoundTripper
}

func (t *rewriteMessagesHost) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "REDACTED-UPSTREAM" && strings.HasPrefix(req.URL.Path, "/v1/messages") {
		if t.inner == nil {
			t.inner = http.DefaultTransport
		}
		cloned := req.Clone(req.Context())
		dest := *t.serverURL
		dest.Path = "/v1/messages"
		dest.RawPath = ""
		cloned.URL = &dest
		return t.inner.RoundTrip(cloned)
	}
	if t.inner == nil {
		t.inner = http.DefaultTransport
	}
	return t.inner.RoundTrip(req)
}

func TestMessageMarshalJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  Message
		want string
	}{
		{
			name: "single_text_block_string_content",
			msg: Message{
				Role:    "user",
				Content: []ContentBlock{{Type: "text", Text: "hello"}},
			},
			want: `{"role":"user","content":"hello"}`,
		},
		{
			name: "mixed_blocks_array_content",
			msg: Message{
				Role: "user",
				Content: []ContentBlock{
					{Type: "text", Text: "see"},
					{Type: "image", Source: &ImageSource{Type: "url", URL: "https://example.com/x.png"}},
				},
			},
			want: `{"role":"user","content":[{"type":"text","text":"see"},{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tt.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}

func TestMessageMarshalJSONWithCacheControlStaysArray(t *testing.T) {
	t.Parallel()
	msg := Message{
		Role: "user",
		Content: []ContentBlock{{
			Type:         "text",
			Text:         "hello",
			CacheControl: &CacheControl{Type: "ephemeral"},
		}},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}`
	if string(raw) != want {
		t.Fatalf("got %s want %s", raw, want)
	}
}

func TestRequestMarshalToolsAndToolChoice(t *testing.T) {
	t.Parallel()
	req := Request{
		Model:     "claude-3-5-sonnet-20241022",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 100,
		Stream:    false,
		Tools: []Tool{
			{
				Name:        "my_tool",
				Description: "does things",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
		ToolChoice: &ToolChoice{
			Type:                   "tool",
			Name:                   "my_tool",
			DisableParallelToolUse: true,
		},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	keys := []string{
		`"tools"`,
		`"tool_choice"`,
		`"type":"tool"`,
		`"name":"my_tool"`,
		`"disable_parallel_tool_use":true`,
		`"input_schema"`,
	}
	for _, k := range keys {
		if !strings.Contains(s, k) {
			t.Fatalf("missing substring %q in %s", k, s)
		}
	}
}

// TestStreamEvents_429InvokesOnHeaders asserts that a 429 response fires the
// OnHeaders callback before returning the error, so the chat handler can
// Claim and inject an in-band overage / early-warning notice into the
// user-facing rate-limit error message.
func TestStreamEvents_429InvokesOnHeaders(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-overage-status", "allowed")
		w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
		w.Header().Set("anthropic-ratelimit-unified-overage-reset", "9999999999")
		w.Header().Set("anthropic-ratelimit-unified-reset", "9999999999")
		w.Header().Set("retry-after", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	t.Cleanup(srv.Close)

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &rewriteMessagesHost{serverURL: srvURL}}
	cli := &Client{
		http:         hc,
		oauth:        &staticToken{},
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:           "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			BetaHeader:            "REDACTED-OAUTH-BETA",
			UserAgent:             "anthropic-test/0",
			CCVersion:             "1.0.0",
			CCEntrypoint:          "test",
			CaptureStore:          seedTestWireBaseline(t),
		},
	}

	var observed http.Header
	_, _, err = cli.StreamEvents(context.Background(), Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
		OnHeaders: func(h http.Header) { observed = h },
	}, func(StreamEvent) error { return nil })
	if err == nil {
		t.Fatalf("StreamEvents returned nil error on 429; want one")
	}
	if !strings.Contains(err.Error(), "anthropic 429") {
		t.Fatalf("err = %q; want anthropic 429 prefix", err.Error())
	}
	// The 429 error must surface as a typed UpstreamError so downstream
	// callers can route on the classification rather than re-parsing the
	// error string.
	ue, ok := AsUpstreamError(err)
	if !ok {
		t.Fatalf("err must be *UpstreamError; got %T (%v)", err, err)
	}
	if ue.Class() != ResponseClassRetryableError {
		t.Fatalf("UpstreamError.Class() = %s; want retryable", ue.Class())
	}
	if !ue.Retryable() {
		t.Fatalf("UpstreamError.Retryable() must be true on 429")
	}
	if ue.Status != http.StatusTooManyRequests {
		t.Fatalf("UpstreamError.Status = %d; want %d", ue.Status, http.StatusTooManyRequests)
	}
	if observed == nil {
		t.Fatalf("OnHeaders was not invoked on 429 response")
	}
	if got := observed.Get("anthropic-ratelimit-unified-status"); got != "rejected" {
		t.Fatalf("OnHeaders received status=%q; want rejected", got)
	}
}

// TestStreamEvents_UpstreamErrorKeepsBodyOutOfLogButInClientMessage pins the
// audit contract: a non-2xx upstream response logs only metadata (no response
// body) to the dedicated anthropic JSONL sink, while the upstream body text
// still reaches the caller through the UpstreamError message. The full body
// lives in the SQLite capture store, which the wire log never restates.
func TestStreamEvents_UpstreamErrorKeepsBodyOutOfLogButInClientMessage(t *testing.T) {
	const sentinel = "anthropic-error-body-sentinel-9a1b"
	upstreamBody := `{"type":"error","error":{"type":"api_error","message":"` + sentinel + `"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(srv.Close)

	logPath := filepath.Join(t.TempDir(), "anthropic.jsonl")
	t.Setenv("CLYDE_ANTHROPIC_LOG_PATH", logPath)
	resetDedicatedAnthropicLoggerForTest(t)

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &rewriteMessagesHost{serverURL: srvURL}}
	cli := &Client{
		http:         hc,
		oauth:        &staticToken{},
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:           "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			BetaHeader:            "REDACTED-OAUTH-BETA",
			UserAgent:             "anthropic-test/0",
			CCVersion:             "1.0.0",
			CCEntrypoint:          "test",
			CaptureStore:          seedTestWireBaseline(t),
		},
	}

	_, _, err = cli.StreamEvents(context.Background(), Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	}, func(StreamEvent) error { return nil })
	if err == nil {
		t.Fatalf("StreamEvents returned nil error on 500; want one")
	}
	ue, ok := AsUpstreamError(err)
	if !ok {
		t.Fatalf("err must be *UpstreamError; got %T (%v)", err, err)
	}
	if !strings.Contains(ue.Message, sentinel) {
		t.Fatalf("client-facing message must carry upstream body text; got %q", ue.Message)
	}

	// Flush the dedicated sink before reading it.
	if fileLoggerCloser != nil {
		if err := fileLoggerCloser.Close(); err != nil {
			t.Fatalf("close anthropic logger: %v", err)
		}
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read anthropic log: %v", err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "anthropic.messages.upstream_error") {
		t.Fatalf("missing upstream_error event in anthropic log: %s", logText)
	}
	if strings.Contains(logText, sentinel) {
		t.Fatalf("error body leaked into anthropic log: %s", logText)
	}
	if strings.Contains(logText, `"body":`) {
		t.Fatalf("anthropic log carries a body field: %s", logText)
	}
}

func TestDoUsesIdentityEncodingForStreams(t *testing.T) {
	t.Parallel()

	var streamEncoding string
	var streamSeen bool
	var nonStreamEncoding string
	var nonStreamSeen bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if payload.Stream {
			streamSeen = true
			streamEncoding = r.Header.Get("Accept-Encoding")
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		} else {
			nonStreamSeen = true
			nonStreamEncoding = r.Header.Get("Accept-Encoding")
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	cli := &Client{
		http:         srv.Client(),
		oauth:        &staticToken{},
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:           srv.URL + "/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			BetaHeader:            "REDACTED-OAUTH-BETA",
			UserAgent:             "anthropic-test/0",
			CCVersion:             "1.0.0",
			CCEntrypoint:          "test",
			CaptureStore:          seedTestWireBaseline(t),
		},
	}
	req := Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	}

	streamReq := req
	streamReq.Stream = true
	resp, err := cli.do(context.Background(), streamReq)
	if err != nil {
		t.Fatalf("stream do: %v", err)
	}
	_ = resp.Body.Close()

	nonStreamReq := req
	nonStreamReq.Stream = false
	resp, err = cli.do(context.Background(), nonStreamReq)
	if err != nil {
		t.Fatalf("non-stream do: %v", err)
	}
	_ = resp.Body.Close()

	if !streamSeen || !nonStreamSeen {
		t.Fatalf("server did not see both requests: stream=%v non_stream=%v", streamSeen, nonStreamSeen)
	}
	if streamEncoding != "identity" {
		t.Fatalf("stream Accept-Encoding = %q; want identity", streamEncoding)
	}
	if nonStreamEncoding != "gzip, deflate, br, zstd" {
		t.Fatalf("non-stream Accept-Encoding = %q; want compressed encodings", nonStreamEncoding)
	}
}

func TestStreamEvents_fixtureSSE(t *testing.T) {
	t.Parallel()
	startPayload, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cbStart, err := json.Marshal(map[string]any{
		"index": 0,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    "t1",
			"name":  "foo",
			"input": map[string]any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cbDelta, err := json.Marshal(map[string]any{
		"index": 0,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": `{"a":1}`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cbStop, err := json.Marshal(map[string]any{"index": 0})
	if err != nil {
		t.Fatal(err)
	}
	msgDelta, err := json.Marshal(map[string]any{
		"delta": map[string]any{"stop_reason": "tool_use"},
		"usage": map[string]any{"output_tokens": 5},
	})
	if err != nil {
		t.Fatal(err)
	}

	sse := strings.Join([]string{
		"event: message_start",
		"data: " + string(startPayload),
		"",
		"event: content_block_start",
		"data: " + string(cbStart),
		"",
		"event: content_block_delta",
		"data: " + string(cbDelta),
		"",
		"event: content_block_stop",
		"data: " + string(cbStop),
		"",
		"event: message_delta",
		"data: " + string(msgDelta),
		"",
		"event: message_stop",
		"data: {}",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &rewriteMessagesHost{serverURL: srvURL}}
	cli := &Client{
		http:         hc,
		oauth:        &staticToken{},
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:             "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion:   "2023-06-01",
			BetaHeader:              "REDACTED-OAUTH-BETA",
			UserAgent:               "anthropic-test/0",
			SystemPromptPrefix:      "",
			StainlessPackageVersion: "0",
			StainlessRuntime:        "node",
			StainlessRuntimeVersion: "v0",
			CCVersion:               "1.0.0",
			CCEntrypoint:            "test",
			CaptureStore:            seedTestWireBaseline(t),
		},
	}

	var got []StreamEvent
	usage, stop, err := cli.StreamEvents(context.Background(), Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	}, func(ev StreamEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if stop != "tool_use" {
		t.Fatalf("stop reason: got %q want tool_use", stop)
	}
	if usage.InputTokens != 1 || usage.OutputTokens != 5 {
		t.Fatalf("usage: got %+v want input 1 output 5", usage)
	}

	want := []StreamEvent{
		StreamToolUseStart{BlockIndex: 0, ToolUseID: "t1", ToolUseName: "foo"},
		StreamToolUseArgDelta{BlockIndex: 0, PartialJSON: `{"a":1}`},
		StreamToolUseStop{BlockIndex: 0},
		StreamStop{StopReason: "tool_use"},
	}
	if len(got) != len(want) {
		t.Fatalf("len got %d want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d: got %#v want %#v", i, got[i], want[i])
		}
	}
}

func TestStreamEvents_fixtureSSEWithCacheUsage(t *testing.T) {
	t.Parallel()
	startPayload, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"usage": map[string]any{
				"input_tokens":                10,
				"output_tokens":               0,
				"cache_creation_input_tokens": 640,
				"cache_read_input_tokens":     3200,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cbStart, err := json.Marshal(map[string]any{
		"index": 0,
		"content_block": map[string]any{
			"type": "text",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cbDelta, err := json.Marshal(map[string]any{
		"index": 0,
		"delta": map[string]any{
			"type": "text_delta",
			"text": "ok",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cbStop, err := json.Marshal(map[string]any{"index": 0})
	if err != nil {
		t.Fatal(err)
	}
	msgDelta, err := json.Marshal(map[string]any{
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{
			"output_tokens":               8,
			"cache_creation_input_tokens": 640,
			"cache_read_input_tokens":     3200,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sse := strings.Join([]string{
		"event: message_start",
		"data: " + string(startPayload),
		"",
		"event: content_block_start",
		"data: " + string(cbStart),
		"",
		"event: content_block_delta",
		"data: " + string(cbDelta),
		"",
		"event: content_block_stop",
		"data: " + string(cbStop),
		"",
		"event: message_delta",
		"data: " + string(msgDelta),
		"",
		"event: message_stop",
		"data: {}",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &rewriteMessagesHost{serverURL: srvURL}}
	cli := &Client{
		http:         hc,
		oauth:        &staticToken{},
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:             "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion:   "2023-06-01",
			BetaHeader:              "REDACTED-OAUTH-BETA",
			UserAgent:               "anthropic-test/0",
			SystemPromptPrefix:      "",
			StainlessPackageVersion: "0",
			StainlessRuntime:        "node",
			StainlessRuntimeVersion: "v0",
			CCVersion:               "1.0.0",
			CCEntrypoint:            "test",
			CaptureStore:            seedTestWireBaseline(t),
		},
	}

	usage, stop, err := cli.StreamEvents(context.Background(), Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	}, func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	if stop != "end_turn" {
		t.Fatalf("stop reason: got %q want end_turn", stop)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 8 {
		t.Fatalf("usage: got %+v want input 10 output 8", usage)
	}
	if usage.CacheCreationInputTokens != 640 {
		t.Fatalf("cache_creation_input_tokens = %d want 640", usage.CacheCreationInputTokens)
	}
	if usage.CacheReadInputTokens != 3200 {
		t.Fatalf("cache_read_input_tokens = %d want 3200", usage.CacheReadInputTokens)
	}
}

// TestStreamEvents_egressCaptureDoesNotAbortScan guards the egress capture
// path, which wraps the upstream SSE body in a captureTee so the exchange lands
// in the SQLite capture store. A regression made captureTee.Read wrap the
// terminal io.EOF with %w, and the bufio.Scanner that consumes the SSE stream
// compares its read error against io.EOF with == (stdlib bufio/scan.go), so the
// wrapped EOF was mistaken for a real failure and StreamEvents returned
// "anthropic stream scan: captureTee read: EOF" on an otherwise healthy 200
// stream. This test drives a full, valid SSE stream with a capture store
// attached and asserts the scan completes and the events parse.
func TestStreamEvents_egressCaptureDoesNotAbortScan(t *testing.T) {
	t.Parallel()
	startPayload, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cbStart, err := json.Marshal(map[string]any{
		"index":         0,
		"content_block": map[string]any{"type": "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cbDelta, err := json.Marshal(map[string]any{
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "pong"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cbStop, err := json.Marshal(map[string]any{"index": 0})
	if err != nil {
		t.Fatal(err)
	}
	msgDelta, err := json.Marshal(map[string]any{
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	sse := strings.Join([]string{
		"event: message_start",
		"data: " + string(startPayload),
		"",
		"event: content_block_start",
		"data: " + string(cbStart),
		"",
		"event: content_block_delta",
		"data: " + string(cbDelta),
		"",
		"event: content_block_stop",
		"data: " + string(cbStop),
		"",
		"event: message_delta",
		"data: " + string(msgDelta),
		"",
		"event: message_stop",
		"data: {}",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)

	srvURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := capture.Open(context.Background(), capture.Config{DBPath: filepath.Join(t.TempDir(), "capture.db")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background(), "test cleanup") })
	seedBaselineIntoStore(t, store)
	hc := &http.Client{Transport: &rewriteMessagesHost{serverURL: srvURL}}
	cli := &Client{
		http:         hc,
		oauth:        &staticToken{},
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:             "https://REDACTED-UPSTREAM/v1/messages",
			OAuthAnthropicVersion:   "2023-06-01",
			BetaHeader:              "REDACTED-OAUTH-BETA",
			UserAgent:               "anthropic-test/0",
			SystemPromptPrefix:      "",
			StainlessPackageVersion: "0",
			StainlessRuntime:        "node",
			StainlessRuntimeVersion: "v0",
			CCVersion:               "1.0.0",
			CCEntrypoint:            "test",
			CaptureStore:            store,
		},
	}

	var got []StreamEvent
	usage, stop, err := cli.StreamEvents(context.Background(), Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "ping"}}}},
		MaxTokens: 10,
	}, func(ev StreamEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamEvents with egress capture returned error on healthy 200 stream: %v", err)
	}
	if stop != "end_turn" {
		t.Fatalf("stop reason: got %q want end_turn", stop)
	}
	if usage.InputTokens != 3 || usage.OutputTokens != 4 {
		t.Fatalf("usage: got %+v want input 3 output 4", usage)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least one stream event, got none")
	}
}
