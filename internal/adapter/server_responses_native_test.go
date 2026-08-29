package adapter

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
)

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

	srv := newNativeResponsesServer(t, upstream.URL, routingTestAuth{})
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

	srv := newNativeResponsesServer(t, upstream.URL, routingTestAuth{})
	front := httptest.NewServer(srv.mux)
	t.Cleanup(front.Close)
	request, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-native","input":"native","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post response: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	<-firstWritten
	readDone := make(chan string, 1)
	go func() {
		buffer := make([]byte, 64)
		count, readErr := response.Body.Read(buffer)
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
