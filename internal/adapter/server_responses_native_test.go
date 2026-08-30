package adapter

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm/capture"
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

func TestNativeCodexResponsesCapturesFourRedactedStages(t *testing.T) {
	const (
		adapterToken      = "adapter-token-must-not-persist"
		configuredOAuth   = "configured-oauth-must-not-persist"
		configuredAccount = "configured-account-must-not-persist"
		requestOAuth      = "request-oauth-must-not-persist"
		requestAccount    = "request-account-must-not-persist"
		requestCookie     = "request-cookie-must-not-persist"
		responseOAuth     = "response-oauth-must-not-persist"
		responseAccount   = "response-account-must-not-persist"
		responseCookie    = "response-cookie-must-not-persist"
	)
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent-stage-marker"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}],"access_token":"` + requestOAuth + `","account_id":"` + requestAccount + `","cookie":"` + requestCookie + `"}`)
	responseBody := []byte(`{"id":"resp-native","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary-stage-marker"}]}],"access_token":"` + responseOAuth + `","account_id":"` + responseAccount + `","cookie":"` + responseCookie + `"}`)
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamBody, _ = io.ReadAll(request.Body)
		writer.Header().Set("Authorization", "Bearer "+responseOAuth)
		writer.Header().Set("Chatgpt-Account-Id", responseAccount)
		writer.Header().Set("Set-Cookie", "session="+responseCookie)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(responseBody)
	}))
	t.Cleanup(upstream.Close)

	dbPath := filepath.Join(t.TempDir(), "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("capture.Open: %v", err)
	}
	auth := &nativeCaptureAuth{token: configuredOAuth, accountID: configuredAccount}
	srv := newNativeResponsesServerWithCapture(t, upstream.URL, auth, store)
	srv.deps.RawResponsesCompaction = adaptercodex.RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+adapterToken)
	request.Header.Set("Cookie", "session="+requestCookie)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeCompactionTurnMetadata())
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)

	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	if bytes.Contains(upstreamBody, []byte("recent-stage-marker")) || !bytes.Contains(upstreamBody, []byte(requestOAuth)) {
		t.Fatalf("upstream body did not preserve the transformed raw request: %s", upstreamBody)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("<pre-compaction-transcript>")) || !bytes.Contains(recorder.Body.Bytes(), []byte(responseOAuth)) {
		t.Fatalf("client body did not preserve the transformed raw response: %s", recorder.Body.Bytes())
	}

	db := openNativeCaptureVerifier(t, dbPath)
	stages := []struct {
		name       string
		client     string
		which      string
		want       string
		wantAbsent string
	}{
		{name: "ingress request", client: "adapter.ingress", which: "request", want: "recent-stage-marker"},
		{name: "upstream request", client: "adapter.codex", which: "request", want: `"text":"old"`, wantAbsent: "recent-stage-marker"},
		{name: "upstream response", client: "adapter.codex", which: "response", want: "summary-stage-marker", wantAbsent: "pre-compaction-transcript"},
		{name: "client response", client: "adapter.ingress", which: "response", want: "pre-compaction-transcript"},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			body := nativeCaptureBody(t, db, stage.client, stage.which)
			if !bytes.Contains(body, []byte(stage.want)) {
				t.Fatalf("capture body %q missing %q", body, stage.want)
			}
			if stage.wantAbsent != "" && bytes.Contains(body, []byte(stage.wantAbsent)) {
				t.Fatalf("capture body %q contains %q", body, stage.wantAbsent)
			}
			assertNativeCaptureSecretsAbsent(t, body)
		})
	}
	rows, err := db.Query(`SELECT req_headers, resp_headers FROM requests WHERE client IN ('adapter.ingress', 'adapter.codex')`)
	if err != nil {
		t.Fatalf("query capture headers: %v", err)
	}
	defer func() { _ = rows.Close() }()
	rowCount := 0
	for rows.Next() {
		var requestHeaders, responseHeaders string
		if err := rows.Scan(&requestHeaders, &responseHeaders); err != nil {
			t.Fatalf("scan capture headers: %v", err)
		}
		assertNativeCaptureSecretsAbsent(t, []byte(requestHeaders+responseHeaders))
		rowCount++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate capture headers: %v", err)
	}
	if rowCount != 2 {
		t.Fatalf("capture rows = %d, want ingress and egress", rowCount)
	}
}

type nativeCaptureAuth struct {
	token     string
	accountID string
}

func (a *nativeCaptureAuth) Token(context.Context) (string, error) {
	return a.token, nil
}

func (a *nativeCaptureAuth) AccountID(context.Context) (string, error) {
	return a.accountID, nil
}

func openNativeCaptureVerifier(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open capture verifier: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func nativeCaptureBody(t *testing.T, db *sql.DB, client string, which string) []byte {
	t.Helper()
	var body []byte
	query := `SELECT data FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE client=?) AND which=?`
	if err := db.QueryRow(query, client, which).Scan(&body); err != nil {
		t.Fatalf("scan %s %s capture body: %v", client, which, err)
	}
	return body
}

func assertNativeCaptureSecretsAbsent(t *testing.T, captured []byte) {
	t.Helper()
	for _, secret := range []string{
		"adapter-token-must-not-persist", "configured-oauth-must-not-persist",
		"configured-account-must-not-persist", "request-oauth-must-not-persist",
		"request-account-must-not-persist", "request-cookie-must-not-persist",
		"response-oauth-must-not-persist", "response-account-must-not-persist",
		"response-cookie-must-not-persist",
	} {
		if bytes.Contains(captured, []byte(secret)) {
			t.Fatalf("capture leaked %q in %q", secret, captured)
		}
	}
}

func TestNativeCodexResponsesCompactionTransformsOnlyTranscriptAndSummary(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent"}]},{ "type":"message", "role":"user", "content":[{"type":"input_text","text":"prompt\\nbytes"}] }],"tools":[{"type":"custom","name":"opaque"}],"metadata":{"keep":true}}`)
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

func TestNativeCodexResponsesCompactionStreamsFirstFrameBeforeCompletion(t *testing.T) {
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"opaque\":true}\n\n")
		writer.(http.Flusher).Flush()
		close(firstWritten)
		<-release
		_, _ = io.WriteString(writer, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}}\n\n")
		_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	srv.deps.RawResponsesCompaction = adaptercodex.RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, FallbackContextWindowTokens: 0,
		MaxTokens: 10_000, ContextWindowFraction: 1, BytesPerToken: 1,
		RecentFraction: 0.5,
	}
	front := httptest.NewServer(srv.mux)
	t.Cleanup(front.Close)
	requestBody := `{"model":"gpt-native","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`
	request, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeCompactionTurnMetadata())
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
		t.Fatal("matching compaction SSE headers waited for upstream completion")
	}
	readDone := make(chan string, 1)
	go func() {
		buffer := make([]byte, len("event: response.created\ndata: {\"type\":\"response.created\",\"opaque\":true}\n\n"))
		count, readErr := io.ReadFull(response.Body, buffer)
		if readErr != nil {
			readDone <- "error: " + readErr.Error()
			return
		}
		readDone <- string(buffer[:count])
	}()
	select {
	case firstFrame := <-readDone:
		if !strings.Contains(firstFrame, "response.created") {
			t.Fatalf("first downstream frame = %q", firstFrame)
		}
	case <-time.After(500 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("matching compaction SSE first frame waited for upstream completion")
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
	return newNativeResponsesServerWithCapture(t, upstreamURL, auth, nil)
}

func newNativeResponsesServerWithCapture(t *testing.T, upstreamURL string, auth adapterprovider.AuthLookup, store *capture.Store) *Server {
	t.Helper()
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.BaseURL = upstreamURL
	cfg.CaptureIngress = store != nil
	cfg.ModelRoutes = []config.AdapterModelRoute{modelRouteForTest("gpt-*", config.AdapterModelProviderCodex, config.AdapterIngressOpenAI)}
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
		GetAuth:      func(adapterresolver.ProviderID) adapterprovider.AuthLookup { return auth },
		CaptureStore: store,
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
