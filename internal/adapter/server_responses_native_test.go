package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/gklog/correlation"
)

func TestNativeCodexResponsesRequestCarriesIngressCorrelation(t *testing.T) {
	correlationContext := correlation.Context{
		TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef",
		ParentSpanID: "fedcba9876543210", RequestID: "req-native", IdentityAttributes: nil,
	}
	raw, model, native := nativeCodexResponsesRequest(
		[]byte(`{"model":"gpt-native","input":"native"}`),
		http.Header{http.CanonicalHeaderKey(adaptercodex.CodexTurnMetadataHeader): {nativeTurnMetadata(t)}},
		correlationContext,
	)
	if !native || model != "gpt-native" || raw.RequestID != "req-native" || raw.Correlation.TraceID != correlationContext.TraceID || raw.Correlation.SpanID != correlationContext.SpanID {
		t.Fatalf("native=%t model=%q raw=%+v", native, model, raw)
	}
}

func TestNativeCodexResponsesPreservesRawRequestAndResponse(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"native"}]}],"tools":[{"type":"custom","name":"apply_patch"}],"metadata":{"opaque":true}}`)
	responseBody := []byte(`{"id":"resp-native","status":"completed","output":[{"type":"custom_tool_call","name":"apply_patch"}]}`)
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotBody, _ = io.ReadAll(request.Body)
		writer.Header().Set("X-Upstream-Marker", "kept")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write(responseBody)
	}))
	t.Cleanup(upstream.Close)

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated || recorder.Header().Get("X-Upstream-Marker") != "kept" || !bytes.Equal(recorder.Body.Bytes(), responseBody) {
		t.Fatalf("status=%d header=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.Bytes())
	}
	if !bytes.Equal(gotBody, requestBody) {
		t.Fatalf("upstream body changed:\n got: %s\nwant: %s", gotBody, requestBody)
	}
}

func TestNativeCodexResponsesCompactionTransformsOnlyTranscriptAndSummary(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent"}]},{ "type":"message", "role":"user", "content":[{"type":"input_text","text":"prompt\\nbytes"}] }],"tools":[{"type":"custom","name":"opaque"}],"metadata":{"keep":true}}`)
	responseBody := []byte(`{"id":"resp-native","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}]}`)
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotBody, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(responseBody)
	}))
	t.Cleanup(upstream.Close)

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	srv.deps.RawResponsesCompaction = adaptercodex.RawResponsesCompactionSettings{
		Enabled:                     true,
		ContextWindowTokens:         0,
		FallbackContextWindowTokens: 10_000,
		MaxTokens:                   10_000,
		ContextWindowFraction:       1,
		BytesPerToken:               1,
		RecentFraction:              0.5,
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeCompactionTurnMetadata())
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if bytes.Contains(gotBody, []byte(`"text":"recent"`)) || !bytes.Contains(gotBody, []byte(`"text":"old"`)) {
		t.Fatalf("upstream transcript split was wrong: %s", gotBody)
	}
	if !bytes.Contains(gotBody, []byte(`{ "type":"message", "role":"user", "content":[{"type":"input_text","text":"prompt\\nbytes"}] }`)) {
		t.Fatalf("upstream prompt bytes changed: %s", gotBody)
	}
	if !bytes.Contains(gotBody, []byte(`"tools":[{"type":"custom","name":"opaque"}]`)) ||
		!bytes.Contains(gotBody, []byte(`"metadata":{"keep":true}`)) {
		t.Fatalf("upstream unrelated fields changed: %s", gotBody)
	}
	if !strings.Contains(recorder.Body.String(), "<pre-compaction-transcript>") ||
		!strings.Contains(recorder.Body.String(), "recent") {
		t.Fatalf("downstream summary missing transcript: %s", recorder.Body.String())
	}
}

func TestNativeCodexResponsesStreamsRawBytes(t *testing.T) {
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "event: raw\\ndata: first\\n\\n")
		writer.(http.Flusher).Flush()
		close(firstWritten)
		<-release
		_, _ = io.WriteString(writer, "event: raw\\ndata: second\\n\\n")
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	front := httptest.NewServer(srv.mux)
	t.Cleanup(front.Close)
	request, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-native","input":"native"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	responseDone := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		response, requestErrValue := http.DefaultClient.Do(request)
		if requestErrValue != nil {
			requestErr <- requestErrValue
			return
		}
		responseDone <- response
	}()
	<-firstWritten
	var response *http.Response
	select {
	case response = <-responseDone:
		t.Cleanup(func() { _ = response.Body.Close() })
	case requestErrValue := <-requestErr:
		t.Fatalf("post response: %v", requestErrValue)
	case <-time.After(500 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("SSE headers waited for upstream completion")
	}
	readDone := make(chan string, 1)
	go func() {
		buffer := make([]byte, len("event: raw\\ndata: first\\n\\n"))
		count, readErr := io.ReadFull(response.Body, buffer)
		if readErr != nil {
			readDone <- "error: " + readErr.Error()
			return
		}
		readDone <- string(buffer[:count])
	}()
	select {
	case got := <-readDone:
		if !strings.Contains(got, "data: first") {
			t.Fatalf("first downstream bytes = %q", got)
		}
	case <-time.After(500 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("first raw bytes waited for upstream completion")
	}
}

func TestNativeCodexResponsesUsesResolvedModelWithoutDroppingFields(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-alias","input":"native","tools":[{"type":"custom","name":"apply_patch"}],"opaque":{"keep":[1,true]}}`)
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotBody, _ = io.ReadAll(request.Body)
		_, _ = writer.Write([]byte(`{"id":"resp-native"}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.BaseURL = upstream.URL
	cfg.Models["gpt-alias"] = config.AdapterModelDeclaration{
		Provider: config.AdapterModelProviderCodex, WireModel: "gpt-resolved", Profile: "haiku",
	}
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
		GetAuth: func(adapterresolver.ProviderID) adapterprovider.AuthLookup { return &nativeRawRefreshAuth{} },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if string(got["model"]) != `"gpt-resolved"` || string(got["tools"]) != `[{"type":"custom","name":"apply_patch"}]` || string(got["opaque"]) != `{"keep":[1,true]}` {
		t.Fatalf("resolved raw body=%s", gotBody)
	}
}

func TestNativeCodexResponsesSSEUpdatesLifecycleStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: done\\n\\n")
	}))
	t.Cleanup(upstream.Close)
	var events []adapterruntime.RequestEvent
	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	srv.deps.RequestEvents = func(_ context.Context, event adapterruntime.RequestEvent) { events = append(events, event) }
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-native","input":"native"}`))
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)
	if len(events) != 3 || events[1].Stage != adapterruntime.RequestStageStreamOpened || !events[1].Stream || !events[2].Stream {
		t.Fatalf("lifecycle events=%+v", events)
	}
}

func TestNativeCodexResponsesRunsControlsBeforeRawDispatch(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		_, _ = writer.Write([]byte(`{"id":"unexpected"}`))
	}))
	t.Cleanup(upstream.Close)

	tests := []struct {
		name     string
		body     string
		header   string
		wantBody string
	}{
		{
			name: "unsupported backend override", body: `{"model":"gpt-native","input":"native"}`,
			header: "invalid", wantBody: "unsupported_backend",
		},
		{
			name: "preflight tool validation", body: `{"model":"gpt-native","input":"native","tools":[{"type":"function","function":{"name":"","parameters":{"type":"object"}}}]}`,
			wantBody: "invalid_tool_name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(test.body))
			request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
			if test.header != "" {
				request.Header.Set("X-Clyde-Backend", test.header)
			}
			recorder := httptest.NewRecorder()
			srv.mux.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.wantBody) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("raw upstream calls=%d, want 0", upstreamCalls.Load())
	}
}

func TestNativeCodexResponsesPreservesInvalidResolverError(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Models["gpt-native"] = config.AdapterModelDeclaration{
		Provider: config.AdapterModelProviderCodex, WireModel: "gpt-resolved", Profile: "haiku",
	}
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
		GetAuth: func(adapterresolver.ProviderID) adapterprovider.AuthLookup { return &nativeRawRefreshAuth{} },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-native","input":"native","reasoning":{"effort":"low"}}`))
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_request_error") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestNativeCodexResponsesEgressUsesDefaultURL(t *testing.T) {
	srv := newNativeResponsesServer(t, "", &nativeRawRefreshAuth{})
	ctx, release := srv.codexRawResponsesEgressContext(context.Background(), "native-request")
	defer release("test")
	if ctx == nil {
		t.Fatal("egress context is nil")
	}
	sessions := srv.egressRegistry.Snapshot()
	if len(sessions) != 1 || sessions[0].Meta.UpstreamURL != "https://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("egress sessions=%+v", sessions)
	}
}

type nativeRawRefreshAuth struct {
	refreshes atomic.Int32
}

func (a *nativeRawRefreshAuth) Token(context.Context) (string, error) {
	return "configured-token", nil
}

func (a *nativeRawRefreshAuth) ForceRefresh(context.Context) (string, error) {
	a.refreshes.Add(1)
	return "refreshed-token", nil
}

func (a *nativeRawRefreshAuth) AccountID(context.Context) (string, error) {
	return "configured-account", nil
}

func TestNativeCodexResponsesTracksEachRawHTTPAttempt(t *testing.T) {
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	var releaseFirstOnce sync.Once
	var releaseSecondOnce sync.Once
	closeFirst := func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) }
	closeSecond := func() { releaseSecondOnce.Do(func() { close(releaseSecond) }) }
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if attempts.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		close(secondStarted)
		<-releaseSecond
		_, _ = writer.Write([]byte(`{"id":"resp-native"}`))
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(closeFirst)
	t.Cleanup(closeSecond)

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-native","input":"native"}`))
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		srv.mux.ServeHTTP(recorder, request)
		responseDone <- recorder
	}()
	<-firstStarted
	assertRawHTTPAttempt(t, srv, 1)
	closeFirst()
	<-secondStarted
	assertRawHTTPAttempt(t, srv, 2)
	closeSecond()
	recorder := <-responseDone
	if recorder.Code != http.StatusOK || attempts.Load() != 2 {
		t.Fatalf("status=%d attempts=%d body=%s", recorder.Code, attempts.Load(), recorder.Body.String())
	}
}

func assertRawHTTPAttempt(t *testing.T, srv *Server, attemptNumber int) {
	t.Helper()
	sessions := srv.egressRegistry.Snapshot()
	if len(sessions) != 2 {
		t.Fatalf("active egress sessions = %d, want parent plus attempt: %+v", len(sessions), sessions)
	}
	var parentID string
	var attemptParentID string
	for _, session := range sessions {
		switch session.Meta.AttemptNo {
		case 0:
			parentID = session.ID
		case attemptNumber:
			if session.ParentID == "" {
				t.Fatalf("attempt %d has no parent: %+v", attemptNumber, session)
			}
			attemptParentID = session.ParentID
		default:
			t.Fatalf("unexpected egress attempt: %+v", session)
		}
	}
	if parentID == "" || attemptParentID != parentID {
		t.Fatalf("parent id=%q attempt parent id=%q", parentID, attemptParentID)
	}
}

func TestResponsesWithoutValidNativeMetadataUsesTypedProjection(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-future","input":"typed","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`))
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, "not-json")
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case typed := <-fakes.codexReqs:
		if typed.Model != "gpt-future" || len(typed.Tools) != 1 || typed.Tools[0].Name != "lookup" {
			t.Fatalf("typed projection = %+v", typed)
		}
	case <-time.After(time.Second):
		t.Fatal("typed Codex projection did not reach the existing transport")
	}
}

func TestNativeTurnMetadataForNonCodexUsesTypedProjection(t *testing.T) {
	fakes := newRoutingFakeEndpoints(t)
	srv := newRoutingIntegrationServer(t, fakes)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-future","input":"typed"}`))
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case typed := <-fakes.anthReqs:
		if typed.Model != "claude-future" {
			t.Fatalf("typed projection model = %q", typed.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("non-Codex request bypassed the existing typed projection")
	}
}

func newNativeResponsesServer(t *testing.T, upstreamURL string, auth adapterprovider.AuthLookup) *Server {
	t.Helper()
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.BaseURL = upstreamURL
	cfg.ModelRoutes = []config.AdapterModelRoute{modelRouteForTest("gpt-*", config.AdapterModelProviderCodex, config.AdapterIngressOpenAI)}
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
		GetAuth: func(adapterresolver.ProviderID) adapterprovider.AuthLookup { return auth },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func nativeTurnMetadata(t *testing.T) string {
	t.Helper()
	metadata, err := adaptercodex.NewTurnMetadata("native-session", "").MarshalCompact()
	if err != nil {
		t.Fatalf("marshal turn metadata: %v", err)
	}
	return metadata
}

func nativeCompactionTurnMetadata() string {
	return `{"session_id":"native-session","thread_source":"user","sandbox":"none","request_kind":"compaction","compaction":{"implementation":"responses"}}`
}
