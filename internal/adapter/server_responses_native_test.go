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
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
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

func TestNativeCodexResponsesZstdPreservesRawExchange(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":"native","metadata":{"opaque":true}}`)
	compressedRequest := zstdEncodeNativeResponseBody(t, requestBody)
	responseBody := []byte(`{"id":"resp-native","status":"completed"}`)
	compressedResponse := zstdEncodeNativeResponseBody(t, responseBody)
	var gotBody []byte
	var gotEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotBody, _ = io.ReadAll(request.Body)
		gotEncoding = request.Header.Get("Content-Encoding")
		writer.Header().Set("Content-Encoding", "zstd")
		_, _ = writer.Write(compressedResponse)
	}))
	t.Cleanup(upstream.Close)

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressedRequest))
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK ||
		recorder.Header().Get("Content-Encoding") != "zstd" ||
		!bytes.Equal(recorder.Body.Bytes(), compressedResponse) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.Bytes())
	}
	if gotEncoding != "zstd" || !bytes.Equal(gotBody, compressedRequest) {
		t.Fatalf("upstream encoding=%q body=%x want encoding=zstd body=%x", gotEncoding, gotBody, compressedRequest)
	}
}

func TestNativeCodexResponsesZstdCompactionPassesThroughOversizedWireResponse(t *testing.T) {
	transformer := nativeCompactionTransformerForTest(t)
	wireBody := bytes.Repeat([]byte("w"), maxResponsesResponseBodyBytes+1)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Encoding": {"zstd"},
			"Content-Type":     {"application/json"},
		},
		Body: io.NopCloser(bytes.NewReader(wireBody)),
	}
	transformed := transformNativeCodexCompactionResponse(response, transformer, false)
	got, err := io.ReadAll(transformed.Body)
	if err != nil {
		t.Fatalf("read preserved wire response: %v", err)
	}
	if transformed.Header.Get("Content-Encoding") != "zstd" || !bytes.Equal(got, wireBody) {
		t.Fatalf("encoding=%q body=%d bytes, want zstd and %d bytes", transformed.Header.Get("Content-Encoding"), len(got), len(wireBody))
	}
}

func TestNativeCodexResponsesZstdCompactionPassesThroughOversizedDecodedResponse(t *testing.T) {
	transformer := nativeCompactionTransformerForTest(t)
	decodedBody := append([]byte(`{"output":[],"padding":"`), bytes.Repeat([]byte("x"), maxResponsesResponseBodyBytes+1)...)
	decodedBody = append(decodedBody, []byte(`"}`)...)
	wireBody := zstdEncodeNativeResponseBody(t, decodedBody)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Encoding": {"zstd"},
			"Content-Type":     {"application/json"},
		},
		Body: io.NopCloser(bytes.NewReader(wireBody)),
	}
	transformed := transformNativeCodexCompactionResponse(response, transformer, false)
	got, err := io.ReadAll(transformed.Body)
	if err != nil {
		t.Fatalf("read preserved decoded-limit response: %v", err)
	}
	if transformed.Header.Get("Content-Encoding") != "zstd" || !bytes.Equal(got, wireBody) {
		t.Fatalf("encoding=%q body=%d bytes, want original compressed response", transformed.Header.Get("Content-Encoding"), len(got))
	}
}

func TestNativeCodexResponsesZstdCompactionPreservesUndecodableStreamingResponse(t *testing.T) {
	transformer := nativeCompactionTransformerForTest(t)
	wireBody := []byte("not-a-zstd-stream")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Encoding": {"zstd"},
			"Content-Type":     {"text/event-stream"},
		},
		Body: io.NopCloser(bytes.NewReader(wireBody)),
	}
	transformed := transformNativeCodexCompactionResponse(response, transformer, true)
	got, err := io.ReadAll(transformed.Body)
	if err != nil {
		t.Fatalf("read preserved malformed stream: %v", err)
	}
	if transformed.Header.Get("Content-Encoding") != "zstd" || !bytes.Equal(got, wireBody) {
		t.Fatalf("encoding=%q body=%q, want original compressed stream", transformed.Header.Get("Content-Encoding"), got)
	}
}

func TestNativeCodexResponsesZstdCompactionBoundsStreamingDecoderMemory(t *testing.T) {
	transformer := nativeCompactionTransformerForTest(t)
	encoder, err := zstd.NewWriter(
		nil,
		zstd.WithWindowSize(2*maxResponsesResponseBodyBytes),
		zstd.WithSingleSegment(false),
	)
	if err != nil {
		t.Fatalf("create oversized-window zstd encoder: %v", err)
	}
	decodedBody := bytes.Repeat([]byte("x"), 2*maxResponsesResponseBodyBytes+1)
	wireBody := encoder.EncodeAll(decodedBody, nil)
	if err := encoder.Close(); err != nil {
		t.Fatalf("close oversized-window zstd encoder: %v", err)
	}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Encoding": {"zstd"},
			"Content-Type":     {"text/event-stream"},
		},
		Body: io.NopCloser(bytes.NewReader(wireBody)),
	}
	transformed := transformNativeCodexCompactionResponse(response, transformer, true)
	got, readErr := io.ReadAll(transformed.Body)
	if readErr != nil {
		t.Fatalf("read preserved oversized-window stream: %v", readErr)
	}
	if transformed.Header.Get("Content-Encoding") != "zstd" || !bytes.Equal(got, wireBody) {
		t.Fatalf("encoding=%q body=%d bytes, want original compressed stream", transformed.Header.Get("Content-Encoding"), len(got))
	}
}

func TestNativeCodexResponsesZstdCapturesDecodedRedactedCopies(t *testing.T) {
	requestSensitiveValue := "request-compressed-sensitive-marker"
	requestBody, err := json.Marshal(map[string]string{
		"model":             "gpt-native",
		"input":             "request-safe",
		"access_" + "token": requestSensitiveValue,
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	compressedRequest := zstdEncodeNativeResponseBody(t, requestBody)
	responseSensitiveValue := "response-compressed-sensitive-marker"
	responseBody, err := json.Marshal(map[string]string{
		"id":                "resp-native",
		"safe":              "response-safe",
		"access_" + "token": responseSensitiveValue,
	})
	if err != nil {
		t.Fatalf("marshal response body: %v", err)
	}
	compressedResponse := zstdEncodeNativeResponseBody(t, responseBody)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamBody, _ := io.ReadAll(request.Body)
		if request.Header.Get("Content-Encoding") != "zstd" ||
			!bytes.Equal(upstreamBody, compressedRequest) {
			t.Errorf("upstream request encoding=%q body=%x", request.Header.Get("Content-Encoding"), upstreamBody)
		}
		writer.Header().Set("Content-Encoding", "zstd")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(compressedResponse)
	}))
	t.Cleanup(upstream.Close)

	dbPath := filepath.Join(t.TempDir(), "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("capture.Open: %v", err)
	}
	srv := newNativeResponsesServerWithCapture(t, upstream.URL, &nativeRawRefreshAuth{}, store)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressedRequest))
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	if recorder.Header().Get("Content-Encoding") != "zstd" ||
		!bytes.Equal(recorder.Body.Bytes(), compressedResponse) {
		t.Fatalf("downstream response encoding=%q body=%x", recorder.Header().Get("Content-Encoding"), recorder.Body.Bytes())
	}

	db := openNativeCaptureVerifier(t, dbPath)
	stages := []struct {
		client         string
		which          string
		safe           string
		sensitiveValue string
	}{
		{client: "adapter.ingress", which: "request", safe: "request-safe", sensitiveValue: requestSensitiveValue},
		{client: "adapter.codex", which: "request", safe: "request-safe", sensitiveValue: requestSensitiveValue},
		{client: "adapter.codex", which: "response", safe: "response-safe", sensitiveValue: responseSensitiveValue},
		{client: "adapter.ingress", which: "response", safe: "response-safe", sensitiveValue: responseSensitiveValue},
	}
	for _, stage := range stages {
		captured := nativeCaptureBody(t, db, stage.client, stage.which)
		if !json.Valid(captured) || !bytes.Contains(captured, []byte(stage.safe)) ||
			bytes.Contains(captured, []byte(stage.sensitiveValue)) {
			t.Fatalf("%s %s capture = %q", stage.client, stage.which, captured)
		}
	}
}

func TestNativeCodexResponsesZstdCompactionTransformsRequestAndResponse(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`)
	compressedRequest := zstdEncodeNativeResponseBody(t, requestBody)
	responseBody := []byte(`{"id":"resp-native","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}]}`)
	compressedResponse := zstdEncodeNativeResponseBody(t, responseBody)
	var upstreamEncodedBody []byte
	var upstreamEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamEncodedBody, _ = io.ReadAll(request.Body)
		upstreamEncoding = request.Header.Get("Content-Encoding")
		writer.Header().Set("Content-Encoding", "zstd")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(compressedResponse)
	}))
	t.Cleanup(upstream.Close)

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	srv.deps.RawResponsesCompaction = adaptercodex.RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressedRequest))
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeCompactionTurnMetadata())
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.Bytes())
	}
	upstreamBody := zstdDecodeNativeResponseBody(t, upstreamEncodedBody)
	if upstreamEncoding != "zstd" || bytes.Contains(upstreamBody, []byte(`"text":"recent"`)) {
		t.Fatalf("upstream encoding=%q body=%s", upstreamEncoding, upstreamBody)
	}
	if recorder.Header().Get("Content-Encoding") != "" {
		t.Fatalf("downstream Content-Encoding=%q", recorder.Header().Get("Content-Encoding"))
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("<pre-compaction-transcript>")) ||
		!bytes.Contains(recorder.Body.Bytes(), []byte("recent")) {
		t.Fatalf("downstream response was not transformed plaintext: %s", recorder.Body.Bytes())
	}
}

func TestNativeCodexResponsesZstdCompactionPreservesOversizedResponse(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`)
	compressedResponse := zstdEncodeNativeResponseBody(t, bytes.Repeat([]byte("x"), maxResponsesResponseBodyBytes+1))
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Encoding", "zstd")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(compressedResponse)
	}))
	t.Cleanup(upstream.Close)

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	srv.deps.RawResponsesCompaction = adaptercodex.RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(zstdEncodeNativeResponseBody(t, requestBody)))
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeCompactionTurnMetadata())
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)

	if recorder.Header().Get("Content-Encoding") != "zstd" || !bytes.Equal(recorder.Body.Bytes(), compressedResponse) {
		t.Fatalf("oversized response changed encoding=%q body=%d", recorder.Header().Get("Content-Encoding"), recorder.Body.Len())
	}
}

func TestNativeCodexResponsesInvalidZstdRedactsAccountDiagnostic(t *testing.T) {
	const accountID = "inbound-account-must-not-leak"
	srv := newNativeResponsesServer(t, "http://127.0.0.1:1", &nativeRawRefreshAuth{})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte("not-zstd")))
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("Chatgpt-Account-Id", accountID)
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest || bytes.Contains(recorder.Body.Bytes(), []byte(accountID)) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.Bytes())
	}
	var response adapteropenai.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal error response: %v body=%s", err, recorder.Body.Bytes())
	}
	if response.Error.Code != "invalid_request" ||
		response.Error.Message != "invalid zstd request body" ||
		response.Error.Clyde == nil ||
		response.Error.Clyde.Headers["chatgpt-account-id"] != "[redacted]" {
		t.Fatalf("error=%+v", response.Error)
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

func TestNativeCodexResponsesExactCompactionRouteTrimsAndInjectsOnce(t *testing.T) {
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
	var upstreamRequest struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(gotBody, &upstreamRequest); err != nil {
		t.Fatalf("unmarshal trimmed upstream request: %v", err)
	}
	if len(upstreamRequest.Input) != 3 {
		t.Fatalf("trimmed upstream input count = %d, want 3: %s", len(upstreamRequest.Input), gotBody)
	}
	if !bytes.Contains(gotBody, []byte(`{ "type":"message", "role":"user", "content":[{"type":"input_text","text":"prompt\\nbytes"}] }`)) {
		t.Fatalf("upstream prompt bytes changed: %s", gotBody)
	}
	if !bytes.Contains(gotBody, []byte(`"tools":[{"type":"custom","name":"opaque"}]`)) ||
		!bytes.Contains(gotBody, []byte(`"metadata":{"keep":true}`)) {
		t.Fatalf("upstream unrelated fields changed: %s", gotBody)
	}
	if strings.Count(recorder.Body.String(), "<pre-compaction-transcript>") != 1 ||
		!strings.Contains(recorder.Body.String(), "recent") {
		t.Fatalf("downstream summary missing transcript: %s", recorder.Body.String())
	}
}

func TestNativeCodexResponsesCompactionV2PassesThrough(t *testing.T) {
	originalRequest := []byte(`{"model":"gpt-native","stream":true,"input":[{"type":"additional_tools","role":"developer"},{"type":"message","role":"developer","content":[{"type":"input_text","text":"setup"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"older"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},{"type":"reasoning","summary":[],"encrypted_content":"cipher"},{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"patch"},{"type":"custom_tool_call_output","call_id":"call-1","output":[{"type":"input_text","text":"result"}]},{"type":"compaction_trigger"}]}`)
	upstreamResponse := []byte("event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"cmp-1\",\"type\":\"compaction\",\"encrypted_content\":\"encrypted-state\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n")
	var upstreamRequest []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamRequest, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write(upstreamResponse)
	}))
	t.Cleanup(upstream.Close)

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	srv.deps.RawResponsesCompaction = adaptercodex.RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(originalRequest))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeCompactionV2TurnMetadata())
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.Bytes())
	}
	if !bytes.Equal(upstreamRequest, originalRequest) {
		t.Fatal("v2 request changed before persistence proof")
	}
	clientResponse := recorder.Body.Bytes()
	if !bytes.Equal(clientResponse, upstreamResponse) {
		t.Fatal("v2 response changed before persistence proof")
	}
}

func TestNativeCodexResponsesCompactionV2RecoveryRequest(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher","opaque":true},{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}],"opaque":{"keep":true}}`)
	responseBody := []byte(`{"id":"resp-1","output":[]}`)
	for _, testCase := range []struct {
		name     string
		body     []byte
		encoding string
	}{
		{name: "non-streaming", body: requestBody},
		{name: "zstd", body: zstdEncodeNativeResponseBody(t, requestBody), encoding: "zstd"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var upstreamBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				upstreamBody, _ = io.ReadAll(request.Body)
				_, _ = writer.Write(responseBody)
			}))
			t.Cleanup(upstream.Close)

			srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
			if !srv.compactionV2.Arm("native-session", "cipher", "recovered transcript") {
				t.Fatal("arm registry")
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(testCase.body))
			request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeFinalAnswerTurnMetadata())
			if testCase.encoding != "" {
				request.Header.Set("Content-Encoding", testCase.encoding)
			}
			recorder := httptest.NewRecorder()
			srv.mux.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), responseBody) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.Bytes())
			}
			if testCase.encoding != "" {
				upstreamBody = zstdDecodeNativeResponseBody(t, upstreamBody)
			}
			assertNativeCompactionV2RecoveryInput(t, upstreamBody)
		})
	}
}

func TestNativeCodexResponsesCompactionV2RecoveryLifecycle(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"}]}`)
	naturalResend := []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<pre-compaction-transcript>recovered transcript</pre-compaction-transcript>"}]}]}`)
	responseBody := []byte(`{"id":"resp-1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final answer"}]}]}`)
	naturalResponse := []byte(`{"id":"resp-2","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<pre-compaction-transcript>recovered transcript</pre-compaction-transcript>\nfinal answer"}]}]}`)
	var upstreamBodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		upstreamBodies = append(upstreamBodies, body)
		if bytes.Contains(body, []byte("<pre-compaction-transcript>")) {
			_, _ = writer.Write(naturalResponse)
			return
		}
		_, _ = writer.Write(responseBody)
	}))
	t.Cleanup(upstream.Close)

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	if !srv.compactionV2.Arm("native-session", "cipher", "recovered transcript") {
		t.Fatal("arm registry")
	}
	for _, body := range [][]byte{requestBody, naturalResend} {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeFinalAnswerTurnMetadata())
		recorder := httptest.NewRecorder()
		srv.mux.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || bytes.Count(recorder.Body.Bytes(), []byte("<pre-compaction-transcript>")) != 1 {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.Bytes())
		}
	}
	if _, ok := srv.compactionV2.Match("native-session", "cipher"); ok {
		t.Fatal("final answer did not complete recovery")
	}
	if len(upstreamBodies) != 2 || bytes.Count(upstreamBodies[0], []byte(`\u003cpre-compaction-transcript\u003e`)) != 1 || !bytes.Equal(upstreamBodies[1], naturalResend) {
		t.Fatalf("upstream bodies = %q", upstreamBodies)
	}
}

func TestNativeCodexResponsesCompactionV2RecoveryServerFailsOpenForNonregularAndDeliveryFailures(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"}]}`)
	sseBody := []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}}\n\n")
	jsonBody := []byte(`{"id":"resp-1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`)
	for _, testCase := range []struct {
		name         string
		metadata     string
		body         []byte
		requestZstd  bool
		responseBody []byte
		responseType string
		responseZstd bool
	}{
		{name: "nonregular json", metadata: `{"session_id":"native-session","thread_source":"user","sandbox":"none","compaction":{"phase":"final_answer"}}`, body: requestBody, responseBody: jsonBody, responseType: "application/json"},
		{name: "nonregular stream", metadata: `{"session_id":"native-session","thread_source":"user","sandbox":"none","request_kind":"turn","compaction":{"implementation":"responses_compaction_v2","phase":"final_answer"}}`, body: requestBody, responseBody: sseBody, responseType: "text/event-stream"},
		{name: "nonregular zstd", metadata: `{"session_id":"native-session","thread_source":"user","sandbox":"none","request_kind":"turn","compaction":{"phase":"final_answer","strategy":"memento"}}`, body: requestBody, requestZstd: true, responseBody: jsonBody, responseType: "application/json", responseZstd: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			wireRequest := testCase.body
			if testCase.requestZstd {
				wireRequest = zstdEncodeNativeResponseBody(t, wireRequest)
			}
			wireResponse := testCase.responseBody
			if testCase.responseZstd {
				wireResponse = zstdEncodeNativeResponseBody(t, wireResponse)
			}
			var upstreamBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				upstreamBody, _ = io.ReadAll(request.Body)
				writer.Header().Set("Content-Type", testCase.responseType)
				if testCase.responseZstd {
					writer.Header().Set("Content-Encoding", "zstd")
				}
				_, _ = writer.Write(wireResponse)
			}))
			t.Cleanup(upstream.Close)
			srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
			if !srv.compactionV2.Arm("native-session", "cipher", "recovered transcript") {
				t.Fatal("arm registry")
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(wireRequest))
			request.Header.Set(adaptercodex.CodexTurnMetadataHeader, testCase.metadata)
			if testCase.requestZstd {
				request.Header.Set("Content-Encoding", "zstd")
			}
			recorder := httptest.NewRecorder()
			srv.mux.ServeHTTP(recorder, request)
			if !bytes.Equal(upstreamBody, wireRequest) || !bytes.Equal(recorder.Body.Bytes(), wireResponse) {
				t.Fatalf("request=%x response=%x", upstreamBody, recorder.Body.Bytes())
			}
			if _, ok := srv.compactionV2.Match("native-session", "cipher"); !ok {
				t.Fatal("pending recovery changed")
			}
		})
	}

	t.Run("downstream write failure", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write(jsonBody) }))
		t.Cleanup(upstream.Close)
		srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
		if !srv.compactionV2.Arm("native-session", "cipher", "recovered transcript") {
			t.Fatal("arm registry")
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
		request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeFinalAnswerTurnMetadata())
		writer := &passthroughPartialWriter{header: make(http.Header), failAfter: 0}
		srv.mux.ServeHTTP(writer, request)
		if _, ok := srv.compactionV2.Match("native-session", "cipher"); !ok {
			t.Fatal("failed client write completed recovery")
		}
	})
}

func TestNativeCodexResponsesCompactionV2OpenDoesNotCompleteOrMutateRecovery(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"}]}`)
	responseBody := []byte(`{"id":"resp-1","output":[]}`)
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamBody, _ = io.ReadAll(request.Body)
		_, _ = writer.Write(responseBody)
	}))
	t.Cleanup(upstream.Close)

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	if !srv.compactionV2.Arm("native-session", "cipher", "recovered transcript") {
		t.Fatal("arm registry")
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Equal(recorder.Body.Bytes(), responseBody) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.Bytes())
	}
	if _, ok := srv.compactionV2.Match("native-session", "cipher"); !ok {
		t.Fatal("OpenRawResponses completed recovery")
	}

	v1Request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	v1Request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeCompactionTurnMetadata())
	recorder = httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, v1Request)
	if recorder.Code != http.StatusOK || !bytes.Equal(upstreamBody, requestBody) {
		t.Fatalf("status=%d upstream=%s", recorder.Code, upstreamBody)
	}
}

func TestNativeCodexResponsesCompactionV2RecoveryRequestFailsOpen(t *testing.T) {
	base := []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"}]}`)
	tests := []struct {
		name     string
		body     []byte
		metadata string
	}{
		{name: "v1 compaction", body: base, metadata: nativeCompactionTurnMetadata()},
		{name: "wrong session", body: base, metadata: `{"session_id":"other","thread_source":"user","sandbox":"none"}`},
		{name: "wrong digest", body: []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"other"}]}`), metadata: nativeTurnMetadata(t)},
		{name: "duplicate compaction", body: []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"},{"type":"compaction","encrypted_content":"cipher"}]}`), metadata: nativeTurnMetadata(t)},
		{name: "malformed input field", body: []byte(`{"model":"gpt-native","input":[],"input":[{"type":"compaction","encrypted_content":"cipher"}]}`), metadata: nativeTurnMetadata(t)},
		{name: "tagged", body: []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<pre-compaction-transcript>kept</pre-compaction-transcript>"}]}]}`), metadata: nativeTurnMetadata(t)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var upstreamBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				upstreamBody, _ = io.ReadAll(request.Body)
				_, _ = writer.Write([]byte(`{"id":"resp-1","output":[]}`))
			}))
			t.Cleanup(upstream.Close)

			srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
			if !srv.compactionV2.Arm("native-session", "cipher", "recovered transcript") {
				t.Fatal("arm registry")
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(testCase.body))
			request.Header.Set(adaptercodex.CodexTurnMetadataHeader, testCase.metadata)
			recorder := httptest.NewRecorder()
			srv.mux.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK || !bytes.Equal(upstreamBody, testCase.body) {
				t.Fatalf("status=%d upstream=%s want=%s", recorder.Code, upstreamBody, testCase.body)
			}
		})
	}
}

func TestNativeCodexResponsesCompactionV2RecoveryRequestFailsOpenZstd(t *testing.T) {
	base := []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"}]}`)
	tests := []struct {
		name     string
		body     []byte
		metadata string
	}{
		{name: "wrong session", body: base, metadata: `{"session_id":"other","thread_source":"user","sandbox":"none"}`},
		{name: "wrong digest", body: []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"other"}]}`), metadata: nativeTurnMetadata(t)},
		{name: "duplicate compaction", body: []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"},{"type":"compaction","encrypted_content":"cipher"}]}`), metadata: nativeTurnMetadata(t)},
		{name: "malformed input field", body: []byte(`{"model":"gpt-native","input":[],"input":[{"type":"compaction","encrypted_content":"cipher"}]}`), metadata: nativeTurnMetadata(t)},
		{name: "tagged", body: []byte(`{"model":"gpt-native","input":[{"type":"compaction","encrypted_content":"cipher"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<pre-compaction-transcript>kept</pre-compaction-transcript>"}]}]}`), metadata: nativeTurnMetadata(t)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var upstreamBody []byte
			var upstreamEncoding string
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				upstreamBody, _ = io.ReadAll(request.Body)
				upstreamEncoding = request.Header.Get("Content-Encoding")
				_, _ = writer.Write([]byte(`{"id":"resp-1","output":[]}`))
			}))
			t.Cleanup(upstream.Close)

			srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
			if !srv.compactionV2.Arm("native-session", "cipher", "recovered transcript") {
				t.Fatal("arm registry")
			}
			wireBody := zstdEncodeNativeResponseBody(t, testCase.body)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(wireBody))
			request.Header.Set("Content-Encoding", "zstd")
			request.Header.Set(adaptercodex.CodexTurnMetadataHeader, testCase.metadata)
			recorder := httptest.NewRecorder()
			srv.mux.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK || upstreamEncoding != "zstd" || !bytes.Equal(upstreamBody, wireBody) {
				t.Fatalf("status=%d encoding=%q upstream=%x want=%x", recorder.Code, upstreamEncoding, upstreamBody, wireBody)
			}
		})
	}
}

func assertNativeCompactionV2RecoveryInput(t *testing.T, body []byte) {
	t.Helper()
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	if len(request.Input) != 3 || !bytes.Contains(request.Input[0], []byte(`"encrypted_content":"cipher"`)) || !bytes.Contains(request.Input[1], []byte(`"role":"assistant"`)) || !bytes.Contains(request.Input[1], []byte(`pre-compaction-transcript`)) || !bytes.Contains(request.Input[2], []byte(`"text":"next"`)) || !bytes.Contains(body, []byte(`"opaque":{"keep":true}`)) {
		t.Fatalf("upstream body = %s", body)
	}
}

func TestNativeCodexResponsesCompactionInjectsWithMultilineUnknownFrame(t *testing.T) {
	requestBody := []byte(`{"model":"gpt-native","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`)
	itemDone := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}}\n\n"
	unknownFrame := "event: response.future\n: keep this exact comment\ndata: {\"type\":\"response.future\",\ndata: \"opaque\":true}\n\n"
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, itemDone+unknownFrame+completed)
	}))
	t.Cleanup(upstream.Close)

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	srv.deps.RawResponsesCompaction = adaptercodex.RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody))
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeCompactionTurnMetadata())
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	if !bytes.Contains(body, []byte(unknownFrame)) {
		t.Fatalf("multiline unknown frame changed: %s", body)
	}
	if bytes.Count(body, []byte("<pre-compaction-transcript>")) != 1 ||
		!bytes.Contains(body, []byte("recent")) {
		t.Fatalf("multiline unknown frame suppressed transcript injection: %s", body)
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
	var response *http.Response
	select {
	case <-firstWritten:
	case response = <-responseDone:
		t.Cleanup(func() { _ = response.Body.Close() })
	case requestErrValue := <-requestErr:
		t.Fatalf("post response: %v", requestErrValue)
	case <-time.After(500 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("matching compaction SSE headers waited for upstream completion")
	}
	if response == nil {
		select {
		case response = <-responseDone:
			t.Cleanup(func() { _ = response.Body.Close() })
		case requestErrValue := <-requestErr:
			t.Fatalf("post response: %v", requestErrValue)
		case <-time.After(500 * time.Millisecond):
			releaseOnce.Do(func() { close(release) })
			t.Fatal("matching compaction SSE headers waited for upstream completion")
		}
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

func TestNativeCodexResponsesZstdCompactionStreamsFirstFrameBeforeCompletion(t *testing.T) {
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Encoding", "zstd")
		writer.Header().Set("Content-Type", "text/event-stream")
		encoder, _ := zstd.NewWriter(writer)
		_, _ = io.WriteString(encoder, "event: response.created\ndata: {\"type\":\"response.created\",\"opaque\":true}\n\n")
		_ = encoder.Flush()
		writer.(http.Flusher).Flush()
		close(firstWritten)
		<-release
		_, _ = io.WriteString(encoder, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}}\n\n")
		_, _ = io.WriteString(encoder, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
		_ = encoder.Close()
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	srv.deps.RawResponsesCompaction = adaptercodex.RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}
	front := httptest.NewServer(srv.mux)
	t.Cleanup(front.Close)
	requestBody := []byte(`{"model":"gpt-native","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`)
	compressedRequest := zstdEncodeNativeResponseBody(t, requestBody)
	request, err := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", bytes.NewReader(compressedRequest))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Encoding", "zstd")
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
	var response *http.Response
	select {
	case <-firstWritten:
	case response = <-responseDone:
		t.Cleanup(func() { _ = response.Body.Close() })
	case requestErrValue := <-requestErr:
		t.Fatalf("post response: %v", requestErrValue)
	case <-time.After(500 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("zstd compaction SSE headers waited for upstream completion")
	}
	if response == nil {
		select {
		case response = <-responseDone:
			t.Cleanup(func() { _ = response.Body.Close() })
		case requestErrValue := <-requestErr:
			t.Fatalf("post response: %v", requestErrValue)
		case <-time.After(500 * time.Millisecond):
			releaseOnce.Do(func() { close(release) })
			t.Fatal("zstd compaction SSE headers waited for upstream completion")
		}
	}
	if response.Header.Get("Content-Encoding") != "" {
		t.Fatalf("downstream Content-Encoding=%q", response.Header.Get("Content-Encoding"))
	}
	readDone := make(chan []byte, 1)
	readErr := make(chan error, 1)
	go func() {
		buffer := make([]byte, len("event: response.created"))
		count, err := io.ReadFull(response.Body, buffer)
		if err != nil {
			readErr <- err
			return
		}
		readDone <- buffer[:count]
	}()
	var firstFrame []byte
	select {
	case firstFrame = <-readDone:
		if !bytes.Contains(firstFrame, []byte("response.created")) {
			t.Fatalf("first downstream frame = %q", firstFrame)
		}
	case err := <-readErr:
		t.Fatalf("read first downstream frame: %v", err)
	case <-time.After(500 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("zstd compaction SSE first frame waited for upstream completion")
	}
	releaseOnce.Do(func() { close(release) })
	remainder, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read remaining downstream frames: %v", err)
	}
	completeResponse := append(firstFrame, remainder...)
	if !bytes.Contains(completeResponse, []byte("<pre-compaction-transcript>")) ||
		!bytes.Contains(completeResponse, []byte("recent")) {
		t.Fatalf("downstream summary missing transcript: %s", completeResponse)
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

func TestNativeTurnMetadataWithUnresolvedModelUsesTypedRejectionPrecedence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		t.Fatal("unresolved model reached native upstream")
	}))
	t.Cleanup(upstream.Close)
	srv := newNativeResponsesServer(t, upstream.URL, &nativeRawRefreshAuth{})
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"unresolved-native","input":"typed","previous_response_id":"resp-previous"}`),
	)
	request.Header.Set(adaptercodex.CodexTurnMetadataHeader, nativeTurnMetadata(t))
	recorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), "previous_response_id is not supported by Clyde") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
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

func nativeCompactionV2TurnMetadata() string {
	return `{"session_id":"native-session","thread_source":"user","sandbox":"none","request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","phase":"mid_turn","strategy":"memento"}}`
}

func nativeFinalAnswerTurnMetadata() string {
	return `{"session_id":"native-session","thread_source":"user","sandbox":"none","request_kind":"turn","compaction":{"phase":"final_answer"}}`
}

func nativeCompactionTransformerForTest(t *testing.T) *adaptercodex.RawResponsesCompactionTransformer {
	t.Helper()
	body := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`)
	_, transformer := adaptercodex.PrepareRawResponsesCompaction(
		adaptercodex.RawResponsesRequest{
			Body:      body,
			Header:    http.Header{adaptercodex.CodexTurnMetadataHeader: {nativeCompactionTurnMetadata()}},
			RequestID: "req-native",
			Stream:    false,
		},
		adaptercodex.RawResponsesCompactionSettings{
			Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
			ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
		},
	)
	if transformer == nil {
		t.Fatal("expected native compaction transformer")
	}
	return transformer
}

func zstdEncodeNativeResponseBody(t *testing.T, body []byte) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("create zstd encoder: %v", err)
	}
	defer encoder.Close()
	return encoder.EncodeAll(body, nil)
}

func zstdDecodeNativeResponseBody(t *testing.T, body []byte) []byte {
	t.Helper()
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("create zstd decoder: %v", err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(body, nil)
	if err != nil {
		t.Fatalf("decode zstd body: %v", err)
	}
	return decoded
}
