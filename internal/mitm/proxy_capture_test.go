package mitm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/slogger"
)

func TestProxyRawHTTPCaptureWritesBoundedIndexWithSidecarRefs(t *testing.T) {
	const largeValue = "raw-index-body-sentinel"
	requestBody := []byte(`{"model":"gpt-test","input":"` + strings.Repeat(largeValue, 256) + `"}`)
	responseBody := []byte(`{"id":"resp-test","output":"` + strings.Repeat("response-body-sentinel", 256) + `"}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		if !bytes.Equal(body, requestBody) {
			t.Errorf("upstream body length = %d want %d", len(body), len(requestBody))
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()

	captureDir := t.TempDir()
	proxy := newHTTPProxyForCaptureTest(t, captureDir, true, upstream)
	req := httptest.NewRequest(http.MethodPost, "http://clyde.test/v1/responses?probe=raw", bytes.NewReader(requestBody))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	proxy.handle(recorder, req)
	resp := recorder.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d want %d", resp.StatusCode, http.StatusCreated)
	}
	gotResponse, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !bytes.Equal(gotResponse, responseBody) {
		t.Fatalf("response body length = %d want %d", len(gotResponse), len(responseBody))
	}

	capturePath := filepath.Join(captureDir, captureIndexFilename)
	rawIndex := readFile(t, capturePath)
	if bytes.Contains(rawIndex, []byte(strings.Repeat(largeValue, 8))) {
		t.Fatalf("capture index contains repeated raw request body")
	}
	if bytes.Contains(rawIndex, []byte(strings.Repeat("response-body-sentinel", 8))) {
		t.Fatalf("capture index contains repeated raw response body")
	}
	records := readCaptureJSONL(t, capturePath)
	captureRecord := firstCaptureRecordWithLeg(t, records, logevent.LegMITMCaptureIndex)
	mitmFields := mitmFieldsFromCaptureRecord(t, captureRecord)
	requestRawPath, ok := mitmFields["raw_request_path"].(string)
	if !ok || requestRawPath == "" {
		t.Fatalf("missing raw_request_path in MITM fields: %#v", mitmFields)
	}
	responseRawPath, ok := mitmFields["raw_response_path"].(string)
	if !ok || responseRawPath == "" {
		t.Fatalf("missing raw_response_path in MITM fields: %#v", mitmFields)
	}
	if _, ok := captureRecord["request_body"].(string); ok {
		t.Fatalf("request_body is raw string, want unified metadata: %#v", captureRecord["request_body"])
	}
	if _, ok := captureRecord["response_body"].(string); ok {
		t.Fatalf("response_body is raw string, want unified metadata: %#v", captureRecord["response_body"])
	}
	assertFileMode(t, requestRawPath, rawCaptureFileMode)
	assertFileMode(t, responseRawPath, rawCaptureFileMode)
	if !bytes.Equal(readFile(t, requestRawPath), requestBody) {
		t.Fatalf("request raw sidecar does not match original body")
	}
	if !bytes.Equal(readFile(t, responseRawPath), responseBody) {
		t.Fatalf("response raw sidecar does not match original body")
	}
	if captureRecord["bytes_in"].(float64) != float64(len(requestBody)) {
		t.Fatalf("bytes_in = %#v want %d", captureRecord["bytes_in"], len(requestBody))
	}
	if captureRecord["bytes_out"].(float64) != float64(len(responseBody)) {
		t.Fatalf("bytes_out = %#v want %d", captureRecord["bytes_out"], len(responseBody))
	}
}

func TestNewProxyAppliesLoggingRequiredLegsFromConfig(t *testing.T) {
	caDir := t.TempDir()
	mitmCfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: filepath.Join(caDir, "clyde-mitm-ca.crt"),
			KeyPath:  filepath.Join(caDir, "clyde-mitm-ca.key"),
		},
	}
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	logBuffer := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	loggingRequest := config.LoggingRequest{
		IncompletePolicy: "warn",
		RequiredLegs: map[string][]string{
			string(logevent.SurfaceMITMIDE): {string(logevent.LegMITMIngress)},
		},
	}
	proxy, err := NewProxy(mitmCfg, loggingRequest, logger, listener)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = proxy.Shutdown(shutdownCtx)
	})

	recorder := proxy.requestLog.Begin(
		logevent.Identity{RequestID: "mitm-required-legs-probe"},
		logevent.Path{Surface: logevent.SurfaceMITMIDE, RouteFamily: logevent.RouteFamilyMITMProxy},
	)
	if recorder == nil {
		t.Fatalf("Begin returned nil")
	}
	recorder.Emit(context.Background(), logevent.Event{Path: logevent.Path{Leg: logevent.LegMITMIngress, Phase: logevent.PhaseStarted}})
	if !recorder.Complete(context.Background()) {
		t.Fatalf("Complete returned false; required_legs from config is [mitm_ingress] which we emitted, so we expect true")
	}

	recorder2 := proxy.requestLog.Begin(
		logevent.Identity{RequestID: "mitm-required-legs-incomplete"},
		logevent.Path{Surface: logevent.SurfaceMITMIDE, RouteFamily: logevent.RouteFamilyMITMProxy},
	)
	if recorder2.Complete(context.Background()) {
		t.Fatalf("Complete returned true with no legs emitted; want false")
	}
	if event := firstCaptureLogEvent(t, logBuffer.String(), "logging.request.incomplete"); event == nil {
		t.Fatalf("expected logging.request.incomplete event after second Complete; got log: %s", logBuffer.String())
	} else if event["incomplete_policy"] != "warn" {
		t.Fatalf("incomplete_policy = %v, want warn", event["incomplete_policy"])
	}
}

func TestBeginHTTPLogRecorderUsesNativeCursorHeadersWhenClydeHeadersMissing(t *testing.T) {
	const identityProviderID ProviderID = ProviderIDCursor
	RegisterProviderFirst(testCursorProvider{id: identityProviderID})
	t.Cleanup(func() {
		UnregisterProvider(identityProviderID)
	})
	logBuffer := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	proxy := &Proxy{
		log:            logger,
		requestLog:     logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		captureWriters: newCaptureWriterCache(logger),
	}
	t.Cleanup(proxy.closeCaptureWriters)

	req := httptest.NewRequest(http.MethodPost, "http://cursor.test/aiserver.v1.DashboardService/GetTeams", strings.NewReader(`{"probe":"cursor-identity"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-request-id", "cursor-native-req")
	req.Header.Set("x-original-request-id", "cursor-native-orig")
	req.Header.Set("x-session-id", "cursor-session-1")
	req.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")

	input := httpCaptureRecordInput{
		config:         config.MITMConfig{CaptureDir: t.TempDir()},
		policy:         CaptureFilePolicy{},
		provider:       "cursor",
		upstreamURL:    "https://api2.cursor.sh/aiserver.v1.DashboardService/GetTeams",
		requestBody:    []byte(`{"probe":"cursor-identity"}`),
		responseBody:   []byte(`{"ok":true}`),
		requestIndex:   newCaptureBodyIndexFromSummary(summarizeBody([]byte(`{"probe":"cursor-identity"}`))),
		responseIndex:  newCaptureBodyIndexFromSummary(summarizeBody([]byte(`{"ok":true}`))),
		responseLen:    int64(len(`{"ok":true}`)),
		duration:       time.Millisecond,
		responseStatus: http.StatusAccepted,
	}

	proxy.recordHTTPCapture(req, http.Header{"Content-Type": []string{"application/json"}}, input)

	event := firstCaptureLogEvent(t, logBuffer.String(), "logging.request.leg")
	if event == nil {
		t.Fatalf("expected logging.request.leg event, got log %s", logBuffer.String())
	}
	if event["provider"] != "cursor" {
		t.Fatalf("provider = %v want cursor", event["provider"])
	}
	if event["request_id"] != "cursor-native-req" {
		t.Fatalf("request_id = %v want cursor-native-req", event["request_id"])
	}
	cursorFacet, ok := event["cursor"].(map[string]any)
	if !ok {
		t.Fatalf("cursor facet missing: %#v", event)
	}
	if cursorFacet["request_id"] != "cursor-native-req" {
		t.Fatalf("cursor request_id = %v want cursor-native-req", cursorFacet["request_id"])
	}
	if event["upstream_request_id"] != "cursor-native-orig" {
		t.Fatalf("upstream_request_id = %v want cursor-native-orig", event["upstream_request_id"])
	}
	if event["session_id"] != "cursor-session-1" {
		t.Fatalf("session_id = %v want cursor-session-1", event["session_id"])
	}
	if event["trace_id"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("trace_id = %v want traceparent trace", event["trace_id"])
	}
}

func TestProxyHTTPCaptureEarlyFailureEmitsRequestErrorWithoutIncomplete(t *testing.T) {
	logBuffer := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	proxy := &Proxy{
		log:            logger,
		requestLog:     logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		captureWriters: newCaptureWriterCache(logger),
	}
	t.Cleanup(proxy.closeCaptureWriters)

	req := httptest.NewRequest(http.MethodPost, "http://clyde.test/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	input := httpCaptureRecordInput{
		config:         config.MITMConfig{CaptureDir: t.TempDir()},
		policy:         CaptureFilePolicy{},
		provider:       "claude",
		upstreamURL:    "https://api.anthropic.com/v1/messages",
		requestBody:    []byte(`{"model":"claude","messages":[]}`),
		responseBody:   nil,
		requestIndex:   captureBodyIndex{},
		responseIndex:  captureBodyIndex{},
		responseLen:    0,
		duration:       time.Millisecond,
		responseStatus: http.StatusBadGateway,
	}

	proxy.recordHTTPFailure(req, http.Header{}, input, httpFailureRecord{
		includePayload:      true,
		includeUpstreamSend: true,
		errorCode:           "upstream_dispatch_failed",
		errorMessage:        "upstream dispatch failed",
	})

	events := captureLogEvents(t, logBuffer.String(), "logging.request.leg")
	foundError := false
	for _, event := range events {
		if event["leg"] == string(logevent.LegRequestError) {
			foundError = true
			if event["error_code"] != "upstream_dispatch_failed" {
				t.Fatalf("error_code = %v want upstream_dispatch_failed", event["error_code"])
			}
		}
	}
	if !foundError {
		t.Fatalf("expected request_error leg in %v", events)
	}
	if event := firstCaptureLogEvent(t, logBuffer.String(), "logging.request.incomplete"); event != nil {
		t.Fatalf("did not expect incomplete request event: %v", event)
	}
}

func TestProxyHTTPCaptureEmitsRequiredRequestLegSequence(t *testing.T) {
	logBuffer := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	captureDir := t.TempDir()
	proxy := &Proxy{
		log:            logger,
		requestLog:     logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		captureWriters: newCaptureWriterCache(logger),
	}
	t.Cleanup(proxy.closeCaptureWriters)
	req := httptest.NewRequest(http.MethodPost, "http://clyde.test/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{"Content-Type": []string{"application/json"}}}
	input := httpCaptureRecordInput{
		config:         config.MITMConfig{CaptureDir: captureDir},
		policy:         CaptureFilePolicy{},
		provider:       "openai",
		upstreamURL:    "https://api.openai.com/v1/responses",
		requestBody:    []byte(`{"model":"gpt-5","input":"hello"}`),
		responseBody:   []byte(`{"ok":true}`),
		requestIndex:   newCaptureBodyIndexFromSummary(summarizeBody([]byte(`{"model":"gpt-5","input":"hello"}`))),
		responseIndex:  newCaptureBodyIndexFromSummary(summarizeBody([]byte(`{"ok":true}`))),
		responseLen:    int64(len(`{"ok":true}`)),
		duration:       time.Millisecond,
		responseStatus: http.StatusAccepted,
	}

	proxy.recordHTTPCapture(req, resp.Header, input)

	seen := make(map[string]bool)
	for _, event := range captureLogEvents(t, logBuffer.String(), "logging.request.leg") {
		if event["surface"] != string(logevent.SurfaceMITMIDE) {
			continue
		}
		leg, ok := event["leg"].(string)
		if ok {
			seen[leg] = true
		}
	}
	for _, leg := range logevent.DefaultRequiredLegs()[logevent.SurfaceMITMIDE] {
		if !seen[string(leg)] {
			t.Fatalf("missing required leg %s in events %v", leg, seen)
		}
	}
	if event := firstCaptureLogEvent(t, logBuffer.String(), "logging.request.incomplete"); event != nil {
		t.Fatalf("did not expect incomplete request event: %v", event)
	}
}

func TestProxyCaptureIndexLockFailureDoesNotFailTraffic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	captureDir := t.TempDir()
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		t.Fatalf("mkdir capture dir: %v", err)
	}
	lock, err := lockCaptureIndex(filepath.Join(captureDir, captureIndexFilename))
	if err != nil {
		t.Fatalf("lock capture index: %v", err)
	}
	defer unlockCaptureIndex(lock)

	proxy := newHTTPProxyForCaptureTest(t, captureDir, false, upstream)
	req := httptest.NewRequest(http.MethodPost, "http://clyde.test/v1/responses", strings.NewReader(`{"input":"hello"}`))
	recorder := httptest.NewRecorder()
	proxy.handle(recorder, req)
	resp := recorder.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d want %d", resp.StatusCode, http.StatusAccepted)
	}
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(gotBody) != `{"ok":true}` {
		t.Fatalf("body = %q want upstream response", string(gotBody))
	}
}

func newHTTPProxyForCaptureTest(t *testing.T, captureDir string, rawCaptureEnabled bool, upstream *httptest.Server) *Proxy {
	t.Helper()
	t.Cleanup(registerTestRoute(t, testRouteOpenAIV1, upstream.URL))
	logger := slog.New(newMITMCaptureTestHandler(t, captureDir))
	proxy := &Proxy{
		log:             logger,
		client:          upstream.Client(),
		dialContext:     (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: nil,
		rawCaptureSeq:   atomic.Uint64{},
		Tunnels:         newTestTunnelRegistry(),
		captureWriters:  newCaptureWriterCache(logger),
		requestLog:      logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: captureDir, RawCaptureEnabled: rawCaptureEnabled},
		base:            "http://[::1]",
		server:          nil,
	}
	t.Cleanup(proxy.closeCaptureWriters)
	return proxy
}

func newMITMCaptureTestHandler(t *testing.T, captureDir string) slog.Handler {
	t.Helper()
	handler := slogger.NewMITMCaptureIndexHandler(slogger.MITMCapturePolicy{
		Enabled: true,
		Root:    captureDir,
		Rotation: slogger.RotationPolicy{
			Enabled: false,
		},
	}, slog.LevelDebug)
	if handler == nil {
		t.Fatalf("MITM capture index handler was not created")
	}
	return handler
}

func mitmFieldsFromCaptureRecord(t *testing.T, record map[string]any) map[string]any {
	t.Helper()
	fields, ok := record["mitm"].(map[string]any)
	if !ok {
		raw, _ := json.Marshal(record["mitm"])
		t.Fatalf("mitm = %s, want metadata object", raw)
	}
	return fields
}

func firstCaptureRecordWithLeg(t *testing.T, records []map[string]any, leg logevent.Leg) map[string]any {
	t.Helper()
	for _, record := range records {
		if record["leg"] == string(leg) {
			return record
		}
	}
	t.Fatalf("missing capture record with leg %s in %#v", leg, records)
	return nil
}

func rawBodyRefFromRecord(t *testing.T, record map[string]any, field string) map[string]any {
	t.Helper()
	value, ok := record[field].(map[string]any)
	if !ok {
		raw, _ := json.Marshal(record[field])
		t.Fatalf("%s = %s, want metadata object", field, raw)
	}
	return value
}

func captureLogEvents(t *testing.T, raw string, message string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	events := make([]map[string]any, 0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event["msg"] == message {
			events = append(events, event)
		}
	}
	return events
}

func firstCaptureLogEvent(t *testing.T, raw string, message string) map[string]any {
	t.Helper()
	events := captureLogEvents(t, raw, message)
	if len(events) == 0 {
		return nil
	}
	return events[0]
}
