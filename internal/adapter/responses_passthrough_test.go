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
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"resp_created"}`)
	}))
	t.Cleanup(upstream.Close)

	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"local-model","input":"hi"}`))
	req.Header.Set(clydeingress.HeaderRequestID, "req-stable")
	const traceID = "0123456789abcdef0123456789abcdef"
	req.Header.Set(clydeingress.HeaderTraceID, traceID)
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
}

func TestBufferedPassthroughUsesStreamingFramingHeaderFilter(t *testing.T) {
	header := http.Header{
		"Connection":        []string{"close, X-Internal", "X-Extra"},
		"Content-Length":    []string{"999"},
		"Transfer-Encoding": []string{"chunked"},
		"X-Extra":           []string{"also-secret"},
		"X-Internal":        []string{"secret"},
		"X-Upstream-Marker": []string{"kept"},
	}
	recorder := httptest.NewRecorder()
	writePassthroughOverrideResponse(recorder, http.StatusCreated, []byte("body"), header)

	if recorder.Header().Get("Connection") != "" || recorder.Header().Get("Transfer-Encoding") != "" ||
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
	if streamRecorder.Header().Get("X-Internal") != "" || streamRecorder.Header().Get("X-Extra") != "" ||
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
	rows, err := db.Query(`SELECT client, path, req_headers, resp_headers FROM requests ORDER BY id`)
	if err != nil {
		t.Fatalf("query captures: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]string)
	responseHeaders := make(map[string]string)
	for rows.Next() {
		var client string
		var path string
		var headers string
		var responseHeader string
		if err := rows.Scan(&client, &path, &headers, &responseHeader); err != nil {
			t.Fatalf("scan capture: %v", err)
		}
		seen[client] = path + " " + headers
		responseHeaders[client] = responseHeader
	}
	if !strings.HasPrefix(seen["adapter.ingress"], "/v1/responses ") {
		t.Fatalf("ingress capture = %q", seen["adapter.ingress"])
	}
	if !strings.HasPrefix(seen["adapter.passthrough"], "/responses ") {
		t.Fatalf("egress capture = %q", seen["adapter.passthrough"])
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
