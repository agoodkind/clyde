package adapter

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/mitm/capture"

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

func TestResponsesPassthroughStreamsBeforeUpstreamCompletion(t *testing.T) {
	firstWritten := make(chan struct{})
	release := make(chan struct{})
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
		close(release)
		t.Fatal("first downstream bytes waited for upstream completion")
	}
	close(release)
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
		_, _ = io.WriteString(w, `{"id":"captured","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	dbPath := filepath.Join(t.TempDir(), "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
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

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open capture database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rows, err := db.Query(`SELECT client, path, req_headers FROM requests ORDER BY id`)
	if err != nil {
		t.Fatalf("query captures: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]string)
	for rows.Next() {
		var client string
		var path string
		var headers string
		if err := rows.Scan(&client, &path, &headers); err != nil {
			t.Fatalf("scan capture: %v", err)
		}
		seen[client] = path + " " + headers
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
