package adapter

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"

	_ "github.com/mattn/go-sqlite3"
	"goodkind.io/clyde/internal/config"
)

// TestResponsesPassthroughForwardsToResponsesEndpoint proves a passthrough
// override model on POST /v1/responses forwards the raw Responses body to the
// upstream's /responses endpoint (the matching endpoint, not /chat/completions)
// and writes the upstream Responses object back verbatim, instead of returning
// an unsupported-backend error.
func TestResponsesPassthroughForwardsToResponsesEndpoint(t *testing.T) {
	var gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_upstream","object":"response","status":"completed","model":"local-model","output":[{"type":"message","id":"msg_u","status":"completed","role":"assistant","content":[{"type":"output_text","text":"upstream ok","annotations":[]}]}]}`)
	}))
	defer upstream.Close()

	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"local-model","input":"say hi","unknown":{"keep":true},"stream":false}`))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses (matching endpoint)", gotPath)
	}
	// The raw Responses body is forwarded, so the upstream sees the Responses
	// shape (input), not a chat-completions messages body.
	if !bytes.Contains(gotBody, []byte(`"input"`)) {
		t.Fatalf("upstream body did not carry the raw Responses input: %s", gotBody)
	}
	if !bytes.Contains(gotBody, []byte(`"unknown":{"keep":true}`)) {
		t.Fatalf("upstream body lost unknown Responses field: %s", gotBody)
	}
	// The upstream Responses object is written back verbatim.
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"resp_upstream"`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`"upstream ok"`)) {
		t.Fatalf("response not forwarded verbatim: %s", rec.Body.String())
	}
}

func TestResponsesPassthroughRewritesOnlyConfiguredModel(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed"}`)
	}))
	t.Cleanup(upstream.Close)

	srv := newNamedPassthroughResponsesServer(t, upstream.URL+"/v1", "wire-model")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"responses-alias","input":"hi","metadata":{"opaque":7}}`))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &fields); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	var model string
	if err := json.Unmarshal(fields["model"], &model); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if model != "wire-model" || string(fields["metadata"]) != `{"opaque":7}` {
		t.Fatalf("upstream body = %s", gotBody)
	}
}

func TestResponsesPassthroughPreservesStatusHeadersAndRequestIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "kept")
		w.Header().Set("Connection", "close")
		w.Header().Set(clydeingress.HeaderRequestID, "upstream-request")
		w.Header().Set(clydeingress.HeaderTraceID, "upstream-trace")
		w.Header().Set(clydeingress.HeaderSpanID, "upstream-span")
		w.Header().Set(clydeingress.HeaderParentSpanID, "upstream-parent")
		w.Header().Set(clydeingress.HeaderTraceparent, "upstream-traceparent")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"resp_created"}`)
	}))
	t.Cleanup(upstream.Close)

	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"local-model","input":"hi"}`))
	req.Header.Set(clydeingress.HeaderRequestID, "req-stable")
	const traceID = "0123456789abcdef0123456789abcdef"
	req.Header.Set(clydeingress.HeaderTraceID, traceID)
	req.Header.Set(clydeingress.HeaderSpanID, "0123456789abcdef")
	req.Header.Set(clydeingress.HeaderParentSpanID, "fedcba9876543210")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated || rec.Header().Get("X-Upstream-Marker") != "kept" {
		t.Fatalf("status=%d headers=%v", rec.Code, rec.Header())
	}
	if rec.Header().Get("Connection") != "" {
		t.Fatalf("transport-owned Connection header forwarded: %v", rec.Header())
	}
	if got := rec.Header().Get(clydeingress.HeaderRequestID); got != "req-stable" {
		t.Fatalf("request id = %q, want req-stable", got)
	}
	if got := rec.Header().Get(clydeingress.HeaderTraceID); got != traceID {
		t.Fatalf("trace id = %q, want %s", got, traceID)
	}
	if got := rec.Header().Get(clydeingress.HeaderSpanID); got == "" || got == "upstream-span" ||
		rec.Header().Get(clydeingress.HeaderParentSpanID) != "fedcba9876543210" ||
		rec.Header().Get(clydeingress.HeaderTraceparent) == "" ||
		rec.Header().Get(clydeingress.HeaderTraceparent) == "upstream-traceparent" {
		t.Fatalf("correlation headers = %v", rec.Header())
	}
}

func TestBufferedPassthroughUsesStreamingFramingHeaderFilter(t *testing.T) {
	header := http.Header{
		"Connection":        []string{"close, X-Internal", "X-Extra"},
		"Content-Length":    []string{"999"},
		"Proxy-Connection":  []string{"keep-alive"},
		"Transfer-Encoding": []string{"chunked"},
		"X-Extra":           []string{"also-secret"},
		"X-Internal":        []string{"secret"},
		"X-Upstream-Marker": []string{"kept"},
	}
	recorder := httptest.NewRecorder()
	writePassthroughOverrideResponse(correlation.Context{}, recorder, http.StatusCreated, []byte("body"), header)

	if recorder.Header().Get("Connection") != "" || recorder.Header().Get("Transfer-Encoding") != "" || recorder.Header().Get("Proxy-Connection") != "" ||
		recorder.Header().Get("X-Internal") != "" || recorder.Header().Get("X-Extra") != "" {
		t.Fatalf("framing headers = %v", recorder.Header())
	}
	if recorder.Header().Get("Content-Length") != "4" || recorder.Header().Get("X-Upstream-Marker") != "kept" {
		t.Fatalf("response headers = %v", recorder.Header())
	}

	streamRecorder := httptest.NewRecorder()
	response := &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("body"))}
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := srv.copyPassthroughResponse(context.Background(), streamRecorder, response, true); err != nil {
		t.Fatalf("copy streaming response: %v", err)
	}
	if streamRecorder.Header().Get("X-Internal") != "" || streamRecorder.Header().Get("X-Extra") != "" || streamRecorder.Header().Get("Proxy-Connection") != "" ||
		streamRecorder.Header().Get("X-Upstream-Marker") != "kept" {
		t.Fatalf("stream response headers = %v", streamRecorder.Header())
	}
}

func TestResponsesPassthroughStreamsBeforeUpstreamCompletion(t *testing.T) {
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() {
		releaseOnce.Do(func() { close(release) })
	}
	t.Cleanup(releaseUpstream)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		close(firstWritten)
		<-release
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n")
	}))
	t.Cleanup(upstream.Close)

	srv := httptest.NewServer(newPassthroughOverrideTestServer(t, upstream.URL+"/v1").mux)
	t.Cleanup(srv.Close)
	request, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(`{"model":"local-model","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	<-firstWritten

	readDone := make(chan string, 1)
	go func() {
		buffer := make([]byte, 48)
		count, readErr := response.Body.Read(buffer)
		if readErr != nil {
			readDone <- "error: " + readErr.Error()
			return
		}
		readDone <- string(buffer[:count])
	}()
	select {
	case first := <-readDone:
		if !strings.Contains(first, "response.created") {
			t.Fatalf("first downstream bytes = %q", first)
		}
	case <-time.After(500 * time.Millisecond):
		releaseUpstream()
		t.Fatal("first downstream bytes waited for upstream completion")
	}
	releaseUpstream()
}

func TestResponsesPassthroughNon2xxUsesOpenAIErrorBoundary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"backend unavailable"}}`)
	}))
	t.Cleanup(upstream.Close)
	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"local-model","input":"hi"}`)))

	var envelope adapteropenai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest || envelope.Error.Type != "invalid_request_error" || !strings.Contains(envelope.Error.Message, "backend unavailable") {
		t.Fatalf("status=%d envelope=%+v", rec.Code, envelope)
	}
}

func TestResponsesPassthroughRedirectUsesOpenAIErrorBoundary(t *testing.T) {
	redirectTargetReached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			redirectTargetReached = true
			_, _ = io.WriteString(w, `{"id":"must-not-follow"}`)
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(upstream.Close)
	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"local-model","input":"hi"}`)))

	var envelope adapteropenai.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, recorder.Body.String())
	}
	if redirectTargetReached || recorder.Code != http.StatusBadRequest || envelope.Error.Type != "invalid_request_error" {
		t.Fatalf("target_reached=%t status=%d envelope=%+v", redirectTargetReached, recorder.Code, envelope)
	}
}

func TestResponsesPassthroughSwitchingProtocolsUsesOpenAIErrorBoundary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	t.Cleanup(upstream.Close)
	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"local-model","input":"hi"}`)))

	var envelope adapteropenai.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusBadRequest || envelope.Error.Type != "invalid_request_error" {
		t.Fatalf("status=%d envelope=%+v", recorder.Code, envelope)
	}
}

func TestResponsesPassthroughRejectsFailedStreamBeforeStreamOpened(t *testing.T) {
	statuses := []int{http.StatusTemporaryRedirect, http.StatusTooManyRequests}
	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"upstream rejected stream"}}`)
			}))
			t.Cleanup(upstream.Close)
			srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
			stages := make([]adapterruntime.RequestStage, 0, 3)
			srv.deps.RequestEvents = func(_ context.Context, event adapterruntime.RequestEvent) {
				stages = append(stages, event.Stage)
			}

			recorder := httptest.NewRecorder()
			srv.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"local-model","input":"hi","stream":true}`)))

			if len(stages) != 2 || stages[0] != adapterruntime.RequestStageStarted || stages[1] != adapterruntime.RequestStageFailed {
				t.Fatalf("lifecycle stages = %v, want started then failed", stages)
			}
		})
	}
}

func TestResponsesPassthroughUsageParsesObjectAndTerminalSSE(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "object", body: `{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":4}}}`},
		{name: "terminal SSE", body: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18,\"input_tokens_details\":{\"cached_tokens\":4}}}}\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := passthroughOverrideUsageFromBody([]byte(test.body))
			if usage.InputTokens != 11 || usage.OutputTokens != 7 || usage.TotalTokens != 18 || usage.CachedTokens() != 4 {
				t.Fatalf("usage = %+v", usage)
			}
		})
	}
}

func TestResponsesProviderAssembledContentUsesTextPartsOnly(t *testing.T) {
	content := json.RawMessage(`[{"type":"text","text":"before"},{"type":"image_url","image_url":{"url":"https://example.invalid/x.png"}},{"type":"text","text":" after"}]`)
	response := &adapteropenai.ChatResponse{Choices: []adapteropenai.ChatChoice{{Message: adapteropenai.ChatMessage{Content: content}}}}
	text, _, _, _ := responsesFieldsFromChatResponse(response)
	if text != "before after" {
		t.Fatalf("text = %q, want only actual text parts", text)
	}
}

func TestResponsesPassthroughResolvedSnapshotSurvivesHotApply(t *testing.T) {
	var mutex sync.Mutex
	paths := make([]string, 0, 2)
	oldUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		paths = append(paths, "old")
		mutex.Unlock()
		_, _ = io.WriteString(w, `{"id":"old"}`)
	}))
	t.Cleanup(oldUpstream.Close)
	newUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		paths = append(paths, "new")
		mutex.Unlock()
		_, _ = io.WriteString(w, `{"id":"new"}`)
	}))
	t.Cleanup(newUpstream.Close)

	srv, err := New(context.Background(), passthroughSnapshotConfig(oldUpstream.URL), config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resolved, err := adapterresolver.Resolve(adapterresolver.IngressOpenAI, responsesCursorRequest("snapshot-alias"), adapterresolver.NewModelRegistryAdapter(srv.modelRegistry()))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := srv.ApplyConfig(passthroughSnapshotConfig(newUpstream.URL)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	first := httptest.NewRecorder()
	srv.forwardPassthroughResponses(first, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "req-old", &resolved, []byte(`{"model":"snapshot-alias","input":"hi"}`))
	second := httptest.NewRecorder()
	srv.mux.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"snapshot-alias","input":"hi"}`)))
	mutex.Lock()
	defer mutex.Unlock()
	if strings.Join(paths, ",") != "old,new" {
		t.Fatalf("targets = %v", paths)
	}
}

func TestResponsesPassthroughCapturesIngressAndSanitizedEgress(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "response-secret")
		w.Header().Set("X-Upstream-Marker", "kept")
		w.Header().Set("X-Request-Id", "resp-request-id")
		_, _ = io.WriteString(w, `{"id":"captured","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	dbPath := filepath.Join(t.TempDir(), "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	storeClosed := false
	t.Cleanup(func() {
		if !storeClosed {
			_ = store.Close(context.Background(), "test cleanup")
		}
	})
	cfg := passthroughSnapshotConfig(upstream.URL)
	cfg.CaptureIngress = true
	override := cfg.PassthroughOverrides["snapshot"]
	override.APIKey = "egress-secret"
	cfg.PassthroughOverrides["snapshot"] = override
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{CaptureStore: store}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"snapshot-alias","input":"capture me"}`))
	request.Header.Set("Authorization", "Bearer ingress-secret")
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	storeClosed = true

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open capture database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rows, err := db.Query(`SELECT client, path, req_headers, resp_headers, upstream_request_id FROM requests ORDER BY id`)
	if err != nil {
		t.Fatalf("query captures: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]string)
	responseHeaders := make(map[string]string)
	upstreamRequestIDs := make(map[string]string)
	for rows.Next() {
		var client string
		var path string
		var headers string
		var responseHeader string
		var upstreamRequestID string
		if err := rows.Scan(&client, &path, &headers, &responseHeader, &upstreamRequestID); err != nil {
			t.Fatalf("scan capture: %v", err)
		}
		seen[client] = path + " " + headers
		responseHeaders[client] = responseHeader
		upstreamRequestIDs[client] = upstreamRequestID
	}
	if !strings.HasPrefix(seen["adapter.ingress"], "/v1/responses ") {
		t.Fatalf("ingress capture = %q", seen["adapter.ingress"])
	}
	if !strings.HasPrefix(seen["adapter.passthrough"], "/responses ") {
		t.Fatalf("egress capture = %q", seen["adapter.passthrough"])
	}
	if upstreamRequestIDs["adapter.passthrough"] != "resp-request-id" {
		t.Fatalf("upstream request id = %q", upstreamRequestIDs["adapter.passthrough"])
	}
	for client, value := range seen {
		if strings.Contains(value, "ingress-secret") || strings.Contains(value, "egress-secret") {
			t.Fatalf("%s capture leaked secret: %s", client, value)
		}
	}
	for _, client := range []string{"adapter.ingress", "adapter.passthrough"} {
		if strings.Contains(responseHeaders[client], "response-secret") ||
			!strings.Contains(responseHeaders[client], "X-Upstream-Marker") {
			t.Fatalf("%s response headers = %q", client, responseHeaders[client])
		}
	}
}

func TestPassthroughStreamCaptureRetainsTerminalUsagePastCap(t *testing.T) {
	prefix := bytes.Repeat([]byte("x"), capture.DefaultMaxBodyBytes+1024)
	padding := strings.Repeat("p", 96*1024)
	terminal := []byte("\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"padding\":\"" + padding + "\",\"response\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18}}}\n\n")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(io.MultiReader(bytes.NewReader(prefix), bytes.NewReader(terminal))),
	}
	recorder := httptest.NewRecorder()
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	result, err := srv.copyPassthroughResponse(context.Background(), recorder, response, true)
	if err != nil {
		t.Fatalf("copy response: %v", err)
	}
	if len(result.body) != capture.DefaultMaxBodyBytes || result.totalBytes != len(prefix)+len(terminal) || !result.truncated {
		t.Fatalf("capture result = body:%d total:%d truncated:%t", len(result.body), result.totalBytes, result.truncated)
	}
	if result.usage.InputTokens != 11 || result.usage.OutputTokens != 7 || result.usage.TotalTokens != 18 {
		t.Fatalf("terminal usage = %+v", result.usage)
	}
}

func TestPassthroughStreamCaptureMarksReaderFailureTruncated(t *testing.T) {
	const partialStream = "data: partial\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &passthroughTestReadCloser{steps: []passthroughStreamReadStep{
			{body: []byte(partialStream), err: io.ErrUnexpectedEOF},
		}},
	}
	recorder := httptest.NewRecorder()
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	result, err := srv.copyPassthroughResponse(context.Background(), recorder, response, true)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("copy error = %v, want unexpected EOF", err)
	}
	if !bytes.Equal(result.body, []byte(partialStream)) || result.totalBytes != len(partialStream) || !result.truncated {
		t.Fatalf("capture result = body:%q total:%d truncated:%t", result.body, result.totalBytes, result.truncated)
	}
}

func TestPassthroughStreamCaptureMarksDownstreamWriteFailureTruncated(t *testing.T) {
	const firstChunk = "data: first\n\n"
	const secondChunk = "data: second\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &passthroughTestReadCloser{steps: []passthroughStreamReadStep{
			{body: []byte(firstChunk)},
			{body: []byte(secondChunk), err: io.EOF},
		}},
	}
	writer := &passthroughPartialWriter{header: make(http.Header), failAfter: 1}
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	result, err := srv.copyPassthroughResponse(context.Background(), writer, response, false)
	if !errors.Is(err, errPassthroughDownstreamWrite) {
		t.Fatalf("copy error = %v, want downstream write error", err)
	}
	wantCaptured := []byte(firstChunk + secondChunk)
	if !bytes.Equal(result.body, wantCaptured) || result.totalBytes != len(wantCaptured) || !result.truncated {
		t.Fatalf("capture result = body:%q total:%d truncated:%t", result.body, result.totalBytes, result.truncated)
	}
	if got := writer.body.String(); got != firstChunk {
		t.Fatalf("downstream body = %q, want %q", got, firstChunk)
	}
}

func TestPassthroughStreamCaptureLeavesSuccessfulBelowCapStreamComplete(t *testing.T) {
	const stream = "data: complete\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	recorder := httptest.NewRecorder()
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	result, err := srv.copyPassthroughResponse(context.Background(), recorder, response, true)
	if err != nil {
		t.Fatalf("copy response: %v", err)
	}
	if !bytes.Equal(result.body, []byte(stream)) || result.totalBytes != len(stream) || result.truncated {
		t.Fatalf("capture result = body:%q total:%d truncated:%t", result.body, result.totalBytes, result.truncated)
	}
}

var errPassthroughDownstreamWrite = errors.New("downstream write failed")

type passthroughStreamReadStep struct {
	body []byte
	err  error
}

type passthroughTestReadCloser struct {
	steps []passthroughStreamReadStep
	index int
}

func (r *passthroughTestReadCloser) Read(destination []byte) (int, error) {
	if r.index >= len(r.steps) {
		return 0, io.EOF
	}
	step := r.steps[r.index]
	r.index++
	return copy(destination, step.body), step.err
}

func (r *passthroughTestReadCloser) Close() error { return nil }

type passthroughPartialWriter struct {
	header    http.Header
	body      bytes.Buffer
	failAfter int
	writes    int
}

func (w *passthroughPartialWriter) Header() http.Header { return w.header }

func (w *passthroughPartialWriter) WriteHeader(_ int) {}

func (w *passthroughPartialWriter) Write(body []byte) (int, error) {
	if w.writes == w.failAfter {
		return 0, errPassthroughDownstreamWrite
	}
	w.writes++
	return w.body.Write(body)
}

func TestPassthroughSSEUsageReadsOnlyTerminalResponseUsage(t *testing.T) {
	padding := strings.Repeat("p", 96*1024)
	payload := []byte("event: response.completed\r\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18,\"input_tokens_details\":{\"cached_tokens\":4}},\"metadata\":{\"input_tokens\":101},\"output\":[{\"content\":{\"output_tokens\":102},\"tool\":{\"total_tokens\":103}}]},\"metadata\":{\"cached_tokens\":104},\"padding\":\"" + padding + "\"}\r\n\r\n")

	parser := newPassthroughSSEUsageParser()
	for offset := 0; offset < len(payload); {
		chunkSize := (offset % 17) + 1
		end := offset + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		parser.Write(payload[offset:end])
		offset = end
	}
	usage := parser.Usage()
	if usage.InputTokens != 11 || usage.OutputTokens != 7 || usage.TotalTokens != 18 || usage.CachedTokens() != 4 {
		t.Fatalf("usage = %+v", usage)
	}

	allocations := testing.AllocsPerRun(3, func() {
		streamParser := newPassthroughSSEUsageParser()
		streamParser.Write(payload)
		_ = streamParser.Usage()
	})
	if allocations > 32 {
		t.Fatalf("parser allocations = %.0f, want bounded allocation behavior", allocations)
	}
}

func TestPassthroughSSEUsageRequiresRootResponseUsage(t *testing.T) {
	rootUsage := `"response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":4}}}`
	nestedUsage := `"metadata":{"response":{"usage":{"input_tokens":101,"output_tokens":102,"total_tokens":203,"input_tokens_details":{"cached_tokens":104}}}},"output":[{"response":{"usage":{"input_tokens":201,"output_tokens":202,"total_tokens":403}}}],"tools":[{"response":{"usage":{"input_tokens":301,"output_tokens":302,"total_tokens":603}}}]`
	tests := []struct {
		name    string
		payload string
	}{
		{name: "nested usage before root response", payload: `{"type":"response.completed","note":"valid \"quoted\" string",` + nestedUsage + `,` + rootUsage + `}`},
		{name: "nested usage after root response", payload: `{"type":"response.completed",` + rootUsage + `,` + nestedUsage + `}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := passthroughTerminalSSEUsage(t, test.payload, "\r\n")
			assertPassthroughUsage(t, usage, 11, 7, 18, 4)
		})
	}
}

func TestPassthroughSSEUsageRejectsMalformedTerminalJSON(t *testing.T) {
	validUsage := `"response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":4}}}`
	deepMetadata := strings.Repeat("[", passthroughMaxJSONDepth+1) + "0" + strings.Repeat("]", passthroughMaxJSONDepth+1)
	tests := []struct {
		name    string
		payload string
		lineEnd string
		want    Usage
	}{
		{name: "valid escaped strings", payload: `{"type":"response.completed","note":"quote \" slash \\ unicode \u263A",` + validUsage + `}`, lineEnd: "\n", want: usageWithValues(11, 7, 18, 4)},
		{name: "missing object close", payload: `{"type":"response.completed",` + validUsage, lineEnd: "\r\n"},
		{name: "mismatched delimiter", payload: `{"type":"response.completed",` + validUsage + `]}`, lineEnd: "\n"},
		{name: "missing comma", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":11 "output_tokens":7}}}`, lineEnd: "\r\n"},
		{name: "missing colon", payload: `{"type":"response.completed","response":{"usage":{"input_tokens" 11}}}`, lineEnd: "\n"},
		{name: "trailing garbage", payload: `{"type":"response.completed",` + validUsage + `} nope`, lineEnd: "\r\n"},
		{name: "unterminated string", payload: `{"type":"response.completed","note":"unterminated,` + validUsage + `}`, lineEnd: "\n"},
		{name: "invalid escape", payload: `{"type":"response.completed","note":"\q",` + validUsage + `}`, lineEnd: "\r\n"},
		{name: "invalid unicode escape", payload: `{"type":"response.completed","note":"\u12G4",` + validUsage + `}`, lineEnd: "\n"},
		{name: "invalid literal", payload: `{"type":"response.completed","flag":truee,` + validUsage + `}`, lineEnd: "\r\n"},
		{name: "invalid number", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":1e,"output_tokens":7}}}`, lineEnd: "\n"},
		{name: "extra root value", payload: `{"type":"response.completed",` + validUsage + `}{}`, lineEnd: "\r\n"},
		{name: "depth overflow", payload: `{"type":"response.completed","metadata":` + deepMetadata + `,` + validUsage + `}`, lineEnd: "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := passthroughTerminalSSEUsage(t, test.payload, test.lineEnd)
			if test.want.TotalTokens != 0 {
				assertPassthroughUsage(t, usage, test.want.InputTokens, test.want.OutputTokens, test.want.TotalTokens, test.want.CachedTokens())
				return
			}
			assertPassthroughUsage(t, usage, 0, 0, 0, 0)
		})
	}
}

func TestPassthroughSSEUsageRejectsNegativeCounters(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "input tokens", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":-11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":4}}}}`},
		{name: "output tokens", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":-7,"total_tokens":18,"input_tokens_details":{"cached_tokens":4}}}}`},
		{name: "total tokens", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":-18,"input_tokens_details":{"cached_tokens":4}}}}`},
		{name: "cached tokens", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":-4}}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := passthroughTerminalSSEUsage(t, test.payload, "\r\n")
			assertPassthroughUsage(t, usage, 0, 0, 0, 0)
		})
	}
}

func TestPassthroughSSEUsageRequiresMatchingTerminalMarkers(t *testing.T) {
	payload := `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":4}}}}`
	tests := []struct {
		name      string
		eventType string
		payload   string
	}{
		{name: "created event with completed JSON type", eventType: "response.created", payload: payload},
		{name: "completed event with created JSON type", eventType: "response.completed", payload: strings.Replace(payload, "response.completed", "response.created", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := passthroughSSEUsage(t, test.eventType, test.payload, "\n")
			assertPassthroughUsage(t, usage, 0, 0, 0, 0)
		})
	}
}

func TestPassthroughSSEUsageAcceptsMatchingCompletedAndIncompleteEvents(t *testing.T) {
	tests := []struct {
		name        string
		eventFields []string
		payload     string
		lineEnd     string
		wantUsage   bool
	}{
		{
			name:        "completed with LF",
			eventFields: []string{"response.completed"},
			payload:     `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
			lineEnd:     "\n",
			wantUsage:   true,
		},
		{
			name:        "incomplete with CRLF",
			eventFields: []string{"response.incomplete"},
			payload:     `{"type":"response.incomplete","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
			lineEnd:     "\r\n",
			wantUsage:   true,
		},
		{
			name:        "last repeated event and JSON type match incomplete",
			eventFields: []string{"response.completed", "response.incomplete"},
			payload:     `{"type":"response.completed","type":"response.incomplete","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
			lineEnd:     "\n",
			wantUsage:   true,
		},
		{
			name:        "incomplete event rejects completed JSON type",
			eventFields: []string{"response.incomplete"},
			payload:     `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
			lineEnd:     "\r\n",
		},
		{
			name:        "completed event rejects incomplete JSON type",
			eventFields: []string{"response.completed"},
			payload:     `{"type":"response.incomplete","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
			lineEnd:     "\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := passthroughSSEFieldsUsage(t, test.eventFields, test.payload, test.lineEnd)
			if test.wantUsage {
				assertPassthroughUsage(t, usage, 11, 7, 18, 0)
				return
			}
			assertPassthroughUsage(t, usage, 0, 0, 0, 0)
		})
	}
}

func TestPassthroughSSEUsageUsesLastEventField(t *testing.T) {
	payload := `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`
	tests := []struct {
		name        string
		eventFields []string
		wantInput   int
		wantOutput  int
		wantTotal   int
	}{
		{name: "completed then created", eventFields: []string{"response.completed", "response.created"}},
		{name: "created then completed", eventFields: []string{"response.created", "response.completed"}, wantInput: 11, wantOutput: 7, wantTotal: 18},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := passthroughSSEFieldsUsage(t, test.eventFields, payload, "\r\n")
			assertPassthroughUsage(t, usage, test.wantInput, test.wantOutput, test.wantTotal, 0)
		})
	}
}

func TestPassthroughSSEUsageTreatsEmptyEventFieldAsFinalValue(t *testing.T) {
	payload := `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`
	tests := []struct {
		name       string
		eventLines []string
		wantInput  int
		wantOutput int
		wantTotal  int
	}{
		{name: "completed then bare event", eventLines: []string{"event: response.completed", "event"}},
		{name: "bare event then completed", eventLines: []string{"event", "event: response.completed"}, wantInput: 11, wantOutput: 7, wantTotal: 18},
		{name: "completed then empty event", eventLines: []string{"event: response.completed", "event:"}},
		{name: "empty event then completed", eventLines: []string{"event:", "event: response.completed"}, wantInput: 11, wantOutput: 7, wantTotal: 18},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := passthroughSSEEventLinesUsage(t, test.eventLines, payload, "\n")
			assertPassthroughUsage(t, usage, test.wantInput, test.wantOutput, test.wantTotal, 0)
		})
	}
}

func TestPassthroughSSEUsageUsesLastRootTypeValue(t *testing.T) {
	usage := `"response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{name: "completed then created", payload: `{"type":"response.completed","type":"response.created",` + usage + `}`},
		{name: "created then completed", payload: `{"type":"response.created","type":"response.completed",` + usage + `}`, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := passthroughTerminalSSEUsage(t, test.payload, "\n")
			if test.want {
				assertPassthroughUsage(t, result, 11, 7, 18, 0)
				return
			}
			assertPassthroughUsage(t, result, 0, 0, 0, 0)
		})
	}
}

func TestPassthroughSSEUsageReplacesOrRejectsRepeatedCounters(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    Usage
	}{
		{name: "later integers replace earlier values", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":1,"input_tokens":11,"output_tokens":2,"output_tokens":7,"total_tokens":3,"total_tokens":18,"input_tokens_details":{"cached_tokens":1},"input_tokens_details":{"cached_tokens":4}}}}`, want: usageWithValues(11, 7, 18, 4)},
		{name: "later decimal input invalidates event", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":11,"input_tokens":1.5}}}`},
		{name: "later overflow output invalidates event", payload: `{"type":"response.completed","response":{"usage":{"output_tokens":7,"output_tokens":999999999999999999999999999999}}}`},
		{name: "later string total invalidates event", payload: `{"type":"response.completed","response":{"usage":{"total_tokens":18,"total_tokens":"invalid"}}}`},
		{name: "later negative cached invalidates event", payload: `{"type":"response.completed","response":{"usage":{"input_tokens_details":{"cached_tokens":4},"input_tokens_details":{"cached_tokens":-5}}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := passthroughTerminalSSEUsage(t, test.payload, "\r\n")
			if test.want.TotalTokens != 0 {
				assertPassthroughUsage(t, result, test.want.InputTokens, test.want.OutputTokens, test.want.TotalTokens, test.want.CachedTokens())
				return
			}
			assertPassthroughUsage(t, result, 0, 0, 0, 0)
		})
	}
}

func TestPassthroughSSEUsageClearsSupersededStructuralValues(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    Usage
	}{
		{name: "response null replaces response object", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}},"response":null}`},
		{name: "response array replaces response object", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":11}},"response":[]}`},
		{name: "usage null replaces usage object", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":11},"usage":null}}`},
		{name: "details null replaces cached details", payload: `{"type":"response.completed","response":{"usage":{"input_tokens_details":{"cached_tokens":4},"input_tokens_details":null}}}`},
		{name: "replacement response object drops prior counters", payload: `{"type":"response.completed","response":{"usage":{"input_tokens":11,"total_tokens":18}},"response":{"usage":{"output_tokens":7}}}`, want: usageWithValues(0, 7, 0, 0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := passthroughTerminalSSEUsage(t, test.payload, "\n")
			if test.want.OutputTokens != 0 {
				assertPassthroughUsage(t, result, test.want.InputTokens, test.want.OutputTokens, test.want.TotalTokens, test.want.CachedTokens())
				return
			}
			assertPassthroughUsage(t, result, 0, 0, 0, 0)
		})
	}
}

func TestPassthroughSSEUsageRejectsTerminalEventTrailingWhitespace(t *testing.T) {
	payload := `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`
	for _, eventLine := range []string{"event: response.completed ", "event: response.completed\t"} {
		t.Run(eventLine, func(t *testing.T) {
			result := passthroughSSEEventLinesUsage(t, []string{eventLine}, payload, "\r\n")
			assertPassthroughUsage(t, result, 0, 0, 0, 0)
		})
	}
}

func TestPassthroughSSEUsageAcceptsOnlyLeadingBOM(t *testing.T) {
	payload := `{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`
	for _, test := range []struct {
		name   string
		stream []byte
		want   Usage
	}{
		{name: "without BOM", stream: []byte("event: response.completed\ndata: " + payload + "\n\n"), want: usageWithValues(11, 7, 18, 0)},
		{name: "leading BOM", stream: append([]byte{0xef, 0xbb, 0xbf}, []byte("event: response.completed\ndata: "+payload+"\n\n")...), want: usageWithValues(11, 7, 18, 0)},
		{name: "middle BOM", stream: []byte("event: response.completed\n\xef\xbb\xbfdata: " + payload + "\n\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			parser := newPassthroughSSEUsageParser()
			for offset := 0; offset < len(test.stream); offset++ {
				parser.Write(test.stream[offset : offset+1])
			}
			usage := parser.Usage()
			if test.want.TotalTokens != 0 {
				assertPassthroughUsage(t, usage, test.want.InputTokens, test.want.OutputTokens, test.want.TotalTokens, test.want.CachedTokens())
				return
			}
			assertPassthroughUsage(t, usage, 0, 0, 0, 0)
		})
	}
}

func passthroughTerminalSSEUsage(t *testing.T, payload string, lineEnd string) Usage {
	return passthroughSSEUsage(t, passthroughTerminalResponseEvent, payload, lineEnd)
}

func passthroughSSEUsage(t *testing.T, eventType string, payload string, lineEnd string) Usage {
	return passthroughSSEFieldsUsage(t, []string{eventType}, payload, lineEnd)
}

func passthroughSSEFieldsUsage(t *testing.T, eventFields []string, payload string, lineEnd string) Usage {
	eventLines := make([]string, 0, len(eventFields))
	for _, eventType := range eventFields {
		eventLines = append(eventLines, "event: "+eventType)
	}
	return passthroughSSEEventLinesUsage(t, eventLines, payload, lineEnd)
}

func passthroughSSEEventLinesUsage(t *testing.T, eventLines []string, payload string, lineEnd string) Usage {
	t.Helper()
	var streamBuilder strings.Builder
	for _, eventLine := range eventLines {
		streamBuilder.WriteString(eventLine)
		streamBuilder.WriteString(lineEnd)
	}
	streamBuilder.WriteString("data: ")
	streamBuilder.WriteString(payload)
	streamBuilder.WriteString(lineEnd)
	streamBuilder.WriteString(lineEnd)
	stream := []byte(streamBuilder.String())
	parser := newPassthroughSSEUsageParser()
	for offset := 0; offset < len(stream); {
		chunkSize := (offset % 13) + 1
		end := offset + chunkSize
		if end > len(stream) {
			end = len(stream)
		}
		parser.Write(stream[offset:end])
		offset = end
	}
	return parser.Usage()
}

func usageWithValues(input int, output int, total int, cached int) Usage {
	return Usage{
		PromptTokens: input, CompletionTokens: output, TotalTokens: total,
		PromptTokensDetails: &PromptTokensDetails{CachedTokens: cached}, InputTokens: input, OutputTokens: output,
		CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0,
	}
}

func assertPassthroughUsage(t *testing.T, usage Usage, input int, output int, total int, cached int) {
	t.Helper()
	if usage.InputTokens != input || usage.OutputTokens != output || usage.TotalTokens != total || usage.CachedTokens() != cached {
		t.Fatalf("usage = %+v, want input=%d output=%d total=%d cached=%d", usage, input, output, total, cached)
	}
}

type failingPassthroughWriter struct {
	header http.Header
	body   bytes.Buffer
}

func (w *failingPassthroughWriter) Header() http.Header { return w.header }

func (w *failingPassthroughWriter) WriteHeader(_ int) {}

func (w *failingPassthroughWriter) Write(body []byte) (int, error) {
	return w.body.Write(body)
}

func (w *failingPassthroughWriter) Flush() {}

func TestResponsesPassthroughCopyFailureUsesTypedStreamBoundary(t *testing.T) {
	writer := &failingPassthroughWriter{header: make(http.Header)}
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := correlation.WithContext(context.Background(), correlation.Context{RequestID: "req-copy-123"})
	resolved := &adapterresolver.ResolvedRequest{}
	err := srv.respondPassthroughStreamCopyError(ctx, writer, resolved, "req-copy-123", time.Now(), errors.New("upstream stream failed"))
	if err != nil {
		t.Fatalf("respond stream copy error: %v", err)
	}
	body := writer.body.String()
	if !strings.Contains(body, `"code":"upstream_network_error"`) ||
		!strings.Contains(body, "req-copy-123") || !strings.Contains(body, `"clyde"`) ||
		!strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream body = %q", writer.body.String())
	}
}

func TestStructuredOutputRetryFailureCapturesMatchingAttemptsAndUsesBoundary(t *testing.T) {
	requestCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"not json"}}]}`)
			return
		}
		w.Header().Set("X-Retry-Attempt", "true")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"retry rejected"}}`)
	}))
	t.Cleanup(upstream.Close)
	dbPath := filepath.Join(t.TempDir(), "retry-capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	storeClosed := false
	t.Cleanup(func() {
		if !storeClosed {
			_ = store.Close(context.Background(), "test cleanup")
		}
	})
	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	srv.deps.CaptureStore = store
	resolved, err := adapterresolver.Resolve(adapterresolver.IngressOpenAI, responsesCursorRequest("local-model"), adapterresolver.NewModelRegistryAdapter(srv.modelRegistry()))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	body := []byte(`{"model":"local-model","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	recorder := httptest.NewRecorder()
	srv.forwardPassthroughOverride(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), &resolved, body)
	var envelope adapteropenai.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusBadRequest || envelope.Error.Type != "invalid_request_error" ||
		!strings.Contains(envelope.Error.Message, "retry rejected") {
		t.Fatalf("status=%d envelope=%+v", recorder.Code, envelope)
	}
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	storeClosed = true

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open capture database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rows, err := db.Query(`SELECT status, resp_headers FROM requests WHERE client = 'adapter.passthrough' ORDER BY id`)
	if err != nil {
		t.Fatalf("query captures: %v", err)
	}
	defer func() { _ = rows.Close() }()
	statuses := make([]int, 0, 2)
	retryHeaders := ""
	for rows.Next() {
		var status int
		var headers string
		if err := rows.Scan(&status, &headers); err != nil {
			t.Fatalf("scan capture: %v", err)
		}
		statuses = append(statuses, status)
		if status == http.StatusTooManyRequests {
			retryHeaders = headers
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate captures: %v", err)
	}
	if requestCount != 2 || len(statuses) != 2 || statuses[0] != http.StatusOK || statuses[1] != http.StatusTooManyRequests ||
		!strings.Contains(retryHeaders, "X-Retry-Attempt") {
		t.Fatalf("requests=%d statuses=%v retry_headers=%q", requestCount, statuses, retryHeaders)
	}
}

func TestStructuredOutputRetryTransportFailureCapturesRequestAttempt(t *testing.T) {
	requestCount := 0
	retryRequestHandled := make(chan struct{})
	var retryRequestBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"not json"}}]}`)
			return
		}
		defer close(retryRequestHandled)
		retryRequestBody, _ = io.ReadAll(r.Body)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijacking")
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("hijack retry request: %v", err)
		}
		_ = connection.Close()
	}))
	t.Cleanup(upstream.Close)

	dbPath := filepath.Join(t.TempDir(), "retry-transport-capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	storeClosed := false
	t.Cleanup(func() {
		if !storeClosed {
			_ = store.Close(context.Background(), "test cleanup")
		}
	})
	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	srv.deps.CaptureStore = store
	stages := make([]adapterruntime.RequestStage, 0, 2)
	srv.deps.RequestEvents = func(_ context.Context, event adapterruntime.RequestEvent) {
		stages = append(stages, event.Stage)
	}
	resolved, err := adapterresolver.Resolve(adapterresolver.IngressOpenAI, responsesCursorRequest("local-model"), adapterresolver.NewModelRegistryAdapter(srv.modelRegistry()))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	body := []byte(`{"model":"local-model","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	recorder := httptest.NewRecorder()
	srv.forwardPassthroughOverride(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), &resolved, body)
	select {
	case <-retryRequestHandled:
	case <-time.After(time.Second):
		t.Fatal("retry upstream handler did not complete within 1s")
	}
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	storeClosed = true

	var envelope adapteropenai.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusBadRequest || envelope.Error.Type != "invalid_request_error" {
		t.Fatalf("status=%d envelope=%+v", recorder.Code, envelope)
	}
	if len(stages) != 2 || stages[0] != adapterruntime.RequestStageStarted || stages[1] != adapterruntime.RequestStageFailed {
		t.Fatalf("lifecycle stages = %v, want started then failed", stages)
	}

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open capture database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rows, err := db.Query(`SELECT r.status, r.host, r.method, r.path, r.request_id, r.req_headers, r.resp_headers, r.req_bytes, COALESCE((SELECT length(data) FROM bodies WHERE request_row_id=r.id AND which='response'), 0), b.data FROM requests r LEFT JOIN bodies b ON b.request_row_id=r.id AND b.which='request' WHERE r.client='adapter.passthrough' ORDER BY r.id`)
	if err != nil {
		t.Fatalf("query captures: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type retryCaptureRow struct {
		status          int
		host            string
		method          string
		path            string
		requestID       string
		headers         string
		responseHeaders string
		requestBytes    int
		responseBytes   int
		body            []byte
	}
	captures := make([]retryCaptureRow, 0, 2)
	for rows.Next() {
		var captureRow retryCaptureRow
		if err := rows.Scan(&captureRow.status, &captureRow.host, &captureRow.method, &captureRow.path, &captureRow.requestID, &captureRow.headers, &captureRow.responseHeaders, &captureRow.requestBytes, &captureRow.responseBytes, &captureRow.body); err != nil {
			t.Fatalf("scan capture: %v", err)
		}
		captures = append(captures, captureRow)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate captures: %v", err)
	}
	if requestCount != 2 || len(captures) != 2 {
		t.Fatalf("requests=%d captures=%d", requestCount, len(captures))
	}
	retry := captures[1]
	if retry.status != 0 || retry.method != http.MethodPost || retry.path != "/v1/chat/completions" || retry.host == "" ||
		retry.requestID == "" || retry.requestID != captures[0].requestID || retry.requestBytes != len(retry.body) ||
		!bytes.Equal(retry.body, retryRequestBody) || retry.responseBytes != 0 ||
		(retry.responseHeaders != "" && retry.responseHeaders != "{}") ||
		!bytes.Contains(retry.body, []byte("respond with ONLY raw JSON")) || strings.Contains(retry.headers, "Bearer") {
		t.Fatalf("retry capture = %+v", retry)
	}
}

func TestResponsesPassthroughPrimaryTransportFailureCapturesSafeAttempt(t *testing.T) {
	requestCount := 0
	requestHandled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(requestHandled)
		requestCount++
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijacking")
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("hijack primary request: %v", err)
		}
		_ = connection.Close()
	}))
	t.Cleanup(upstream.Close)

	dbPath := filepath.Join(t.TempDir(), "primary-transport-capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	storeClosed := false
	t.Cleanup(func() {
		if !storeClosed {
			_ = store.Close(context.Background(), "test cleanup")
		}
	})
	const userinfoMarker = "url-user-marker"
	const queryMarker = "url-query-marker"
	sensitiveBaseURL := strings.Replace(upstream.URL, "http://", "http://"+userinfoMarker+"@", 1) + "/v1?marker=" + queryMarker
	var logs bytes.Buffer
	stages := make([]adapterruntime.RequestEvent, 0, 2)
	cfg := baseConfig()
	cfg.Enabled = true
	cfg.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{BaseURL: sensitiveBaseURL}
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
		CaptureStore: store,
		RequestEvents: func(_ context.Context, event adapterruntime.RequestEvent) {
			stages = append(stages, event)
		},
	}, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	body := `{"model":"local-model","input":"capture primary transport failure"}`
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	select {
	case <-requestHandled:
	case <-time.After(time.Second):
		t.Fatal("upstream handler did not complete within 1s")
	}
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	storeClosed = true

	if recorder.Code != http.StatusBadRequest || len(stages) != 2 || stages[1].Stage != adapterruntime.RequestStageFailed {
		t.Fatalf("status=%d stages=%v", recorder.Code, stages)
	}
	if strings.Contains(recorder.Body.String(), userinfoMarker) || strings.Contains(recorder.Body.String(), queryMarker) ||
		strings.Contains(logs.String(), userinfoMarker) || strings.Contains(logs.String(), queryMarker) ||
		strings.Contains(stages[1].Err, userinfoMarker) || strings.Contains(stages[1].Err, queryMarker) {
		t.Fatalf("transport diagnostics leaked configured URL secret")
	}

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open capture database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var status int
	var host string
	var path string
	var requestHeaders string
	var responseHeaders string
	var requestBody []byte
	row := db.QueryRow(`SELECT r.status, r.host, r.path, r.req_headers, r.resp_headers, b.data FROM requests r LEFT JOIN bodies b ON b.request_row_id=r.id AND b.which='request' WHERE r.client='adapter.passthrough'`)
	if err := row.Scan(&status, &host, &path, &requestHeaders, &responseHeaders, &requestBody); err != nil {
		t.Fatalf("scan primary capture: %v", err)
	}
	captureMetadata := host + path + requestHeaders + responseHeaders + string(requestBody)
	if requestCount != 1 || status != 0 || path != "/v1/responses" || !bytes.Equal(requestBody, []byte(body)) ||
		strings.Contains(captureMetadata, userinfoMarker) || strings.Contains(captureMetadata, queryMarker) {
		t.Fatalf("primary capture status=%d host=%q path=%q metadata=%q", status, host, path, captureMetadata)
	}
}

func TestResponsesPassthroughBodyReadFailureCapturesPartialResponse(t *testing.T) {
	const partialBody = `{"id":"partial"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "128")
		_, _ = io.WriteString(w, partialBody)
	}))
	t.Cleanup(upstream.Close)

	dbPath := filepath.Join(t.TempDir(), "primary-read-capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	storeClosed := false
	t.Cleanup(func() {
		if !storeClosed {
			_ = store.Close(context.Background(), "test cleanup")
		}
	})
	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	srv.deps.CaptureStore = store
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"local-model","input":"read failure"}`)))
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	storeClosed = true
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open capture database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var status int
	var responseBytes int
	var truncated int
	var capturedBody []byte
	row := db.QueryRow(`SELECT r.status, r.resp_bytes, b.truncated, b.data FROM requests r JOIN bodies b ON b.request_row_id=r.id AND b.which='response' WHERE r.client='adapter.passthrough'`)
	if err := row.Scan(&status, &responseBytes, &truncated, &capturedBody); err != nil {
		t.Fatalf("scan partial capture: %v", err)
	}
	if status != http.StatusOK || responseBytes != len(partialBody) || truncated != 1 || !bytes.Equal(capturedBody, []byte(partialBody)) {
		t.Fatalf("partial capture status=%d bytes=%d truncated=%d body=%q", status, responseBytes, truncated, capturedBody)
	}
}

func TestStructuredOutputRetryBodyReadFailureCapturesPartialResponse(t *testing.T) {
	const partialBody = `{"choices":[`
	requestCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"not json"}}]}`)
			return
		}
		w.Header().Set("Content-Length", "128")
		_, _ = io.WriteString(w, partialBody)
	}))
	t.Cleanup(upstream.Close)

	dbPath := filepath.Join(t.TempDir(), "retry-read-capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	storeClosed := false
	t.Cleanup(func() {
		if !storeClosed {
			_ = store.Close(context.Background(), "test cleanup")
		}
	})
	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	srv.deps.CaptureStore = store
	resolved, err := adapterresolver.Resolve(adapterresolver.IngressOpenAI, responsesCursorRequest("local-model"), adapterresolver.NewModelRegistryAdapter(srv.modelRegistry()))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	recorder := httptest.NewRecorder()
	body := []byte(`{"model":"local-model","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	srv.forwardPassthroughOverride(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), &resolved, body)
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	storeClosed = true
	if recorder.Code != http.StatusBadRequest || requestCount != 2 {
		t.Fatalf("status=%d requests=%d body=%s", recorder.Code, requestCount, recorder.Body.String())
	}

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open capture database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var status int
	var responseBytes int
	var truncated int
	var capturedBody []byte
	row := db.QueryRow(`SELECT r.status, r.resp_bytes, b.truncated, b.data FROM requests r JOIN bodies b ON b.request_row_id=r.id AND b.which='response' WHERE r.client='adapter.passthrough' ORDER BY r.id DESC LIMIT 1`)
	if err := row.Scan(&status, &responseBytes, &truncated, &capturedBody); err != nil {
		t.Fatalf("scan retry partial capture: %v", err)
	}
	if status != http.StatusOK || responseBytes != len(partialBody) || truncated != 1 || !bytes.Equal(capturedBody, []byte(partialBody)) {
		t.Fatalf("retry partial capture status=%d bytes=%d truncated=%d body=%q", status, responseBytes, truncated, capturedBody)
	}
}

func newNamedPassthroughResponsesServer(t *testing.T, baseURL string, wireModel string) *Server {
	t.Helper()
	cfg := passthroughSnapshotConfig(baseURL)
	override := cfg.PassthroughOverrides["snapshot"]
	override.Model = wireModel
	cfg.PassthroughOverrides["snapshot"] = override
	cfg.Models["responses-alias"] = cfg.Models["snapshot-alias"]
	declaration := cfg.Models["responses-alias"]
	declaration.WireModel = wireModel
	cfg.Models["responses-alias"] = declaration
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func responsesCursorRequest(model string) adaptercursor.Request {
	return adaptercursor.Request{OpenAI: adapteropenai.ChatRequest{Model: model}}
}
