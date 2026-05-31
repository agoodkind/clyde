package mitm

import (
	"bytes"
	"context"
	"database/sql"
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
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog"
)

// TestProxyHTTPCapturePersistsExchangeToStore drives a plain-HTTP request
// through proxy.handle and asserts the two-surface split: the MITM wire concern
// JSONL stays a bounded metadata index (no repeated payload bytes), while the
// full request and response bodies persist to the SQLite capture store and are
// recoverable byte-for-byte by joining requests to bodies.
func TestProxyHTTPCapturePersistsExchangeToStore(t *testing.T) {
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
	dbPath := filepath.Join(captureDir, "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	proxy := newHTTPProxyForCaptureTest(t, captureDir, store, upstream)
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

	// The wire concern JSONL must stay a bounded metadata index: the repeated
	// request and response payloads belong only in the SQLite store.
	capturePath := mitmWireConcernTestPath(captureDir)
	waitForCaptureRecordWithLeg(t, capturePath, logevent.LegMITMCaptureIndex)
	rawIndex := readFile(t, capturePath)
	if bytes.Contains(rawIndex, []byte(strings.Repeat(largeValue, 8))) {
		t.Fatalf("capture index contains repeated raw request body")
	}
	if bytes.Contains(rawIndex, []byte(strings.Repeat("response-body-sentinel", 8))) {
		t.Fatalf("capture index contains repeated raw response body")
	}

	// Close flushes the writer queue so the row is committed before we query it.
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var (
		rowID     int64
		gotClient string
		reqBytes  int64
		respBytes int64
	)
	row := db.QueryRow(`SELECT id, client, req_bytes, resp_bytes FROM requests ORDER BY ts DESC LIMIT 1`)
	if err := row.Scan(&rowID, &gotClient, &reqBytes, &respBytes); err != nil {
		t.Fatalf("scan request row: %v", err)
	}
	if gotClient != "test" {
		t.Fatalf("client = %q want test", gotClient)
	}
	if reqBytes != int64(len(requestBody)) {
		t.Fatalf("req_bytes = %d want %d", reqBytes, len(requestBody))
	}
	if respBytes != int64(len(responseBody)) {
		t.Fatalf("resp_bytes = %d want %d", respBytes, len(responseBody))
	}

	var storedRequest []byte
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=? AND which='request'`, rowID).Scan(&storedRequest); err != nil {
		t.Fatalf("scan request body: %v", err)
	}
	if !bytes.Equal(storedRequest, requestBody) {
		t.Fatalf("stored request body mismatch: got %d bytes want %d", len(storedRequest), len(requestBody))
	}
	var storedResponse []byte
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=? AND which='response'`, rowID).Scan(&storedResponse); err != nil {
		t.Fatalf("scan response body: %v", err)
	}
	if !bytes.Equal(storedResponse, responseBody) {
		t.Fatalf("stored response body mismatch: got %d bytes want %d", len(storedResponse), len(responseBody))
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
	proxy, err := NewProxy(mitmCfg, loggingRequest, logger, []net.Listener{listener}, nil, "test")
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
		log:        logger,
		requestLog: logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
	}

	req := httptest.NewRequest(http.MethodPost, "http://cursor.test/aiserver.v1.DashboardService/GetTeams", strings.NewReader(`{"probe":"cursor-identity"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-request-id", "cursor-native-req")
	req.Header.Set("x-original-request-id", "cursor-native-orig")
	req.Header.Set("x-session-id", "cursor-session-1")
	req.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")

	input := httpCaptureRecordInput{
		config:         config.MITMConfig{CaptureDir: t.TempDir()},
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
		log:        logger,
		requestLog: logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
	}

	req := httptest.NewRequest(http.MethodPost, "http://clyde.test/v1/messages", strings.NewReader(`{"model":"claude","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	input := httpCaptureRecordInput{
		config:         config.MITMConfig{CaptureDir: t.TempDir()},
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
		log:        logger,
		requestLog: logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
	}
	req := httptest.NewRequest(http.MethodPost, "http://clyde.test/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{"Content-Type": []string{"application/json"}}}
	input := httpCaptureRecordInput{
		config:         config.MITMConfig{CaptureDir: captureDir},
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

// TestCaptureIndexSerializesConcurrentWritersWithoutDropping asserts the
// post-collapse guarantee: the single MITM wire concern file takes a
// blocking cross-process flock per write (via gklog.FileJSON's
// NewLockedWriteCloser), so concurrent writers to the same
// providers/mitm/wire.jsonl serialize and every record persists. The old
// non-blocking lock would drop a record on contention; this test fails if any
// record is lost.
func TestCaptureIndexSerializesConcurrentWritersWithoutDropping(t *testing.T) {
	concernRoot := t.TempDir()
	if err := os.MkdirAll(concernRoot, 0o755); err != nil {
		t.Fatalf("mkdir concern root: %v", err)
	}

	const writers = 8
	const recordsPerWriter = 16
	// One shared handler instance is the production shape: a single
	// process owns one wire file handler. The blocking flock inside
	// gklog.FileJSON serializes the concurrent Handle calls so every
	// record lands intact rather than dropping on a non-blocking lock.
	logger := slog.New(newMITMCaptureTestHandler(t, concernRoot))
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(writerIndex int) {
			defer wg.Done()
			for r := 0; r < recordsPerWriter; r++ {
				logger.Info("logging.request.leg",
					slog.String("concern", slogger.ConcernProviderMITMWire),
					slog.Int("writer", writerIndex),
					slog.Int("record", r),
				)
			}
		}(w)
	}
	wg.Wait()

	capturePath := mitmWireConcernTestPath(concernRoot)
	records := readCaptureJSONL(t, capturePath)
	if len(records) != writers*recordsPerWriter {
		t.Fatalf("capture records = %d want %d (records dropped under contention)", len(records), writers*recordsPerWriter)
	}
	seen := make(map[[2]int]bool, writers*recordsPerWriter)
	for _, record := range records {
		writerValue, okWriter := record["writer"].(float64)
		recordValue, okRecord := record["record"].(float64)
		if !okWriter || !okRecord {
			t.Fatalf("capture record missing writer/record fields: %#v", record)
		}
		key := [2]int{int(writerValue), int(recordValue)}
		if seen[key] {
			t.Fatalf("duplicate capture record %v", key)
		}
		seen[key] = true
	}
	if len(seen) != writers*recordsPerWriter {
		t.Fatalf("unique capture records = %d want %d", len(seen), writers*recordsPerWriter)
	}
}

func newHTTPProxyForCaptureTest(t *testing.T, captureDir string, store *capture.Store, upstream *httptest.Server) *Proxy {
	t.Helper()
	t.Cleanup(registerTestRoute(t, testRouteOpenAIV1, upstream.URL))
	logger := slog.New(newMITMCaptureTestHandler(t, captureDir))
	proxy := &Proxy{
		log:             logger,
		httpClient:      upstream.Client(),
		dialContext:     (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: nil,
		store:           store,
		client:          "test",
		Tunnels:         newTestTunnelRegistry(),
		requestLog:      logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: captureDir},
		base:            "http://[::1]",
		server:          nil,
	}
	return proxy
}

// mitmWireConcernTestPath resolves the per-concern MITM wire JSONL file under
// concernRoot, mirroring the slogger router layout
// (providers/mitm/wire.jsonl). The capture sink that wrote a dedicated
// capture.jsonl is gone; MITM legs now land in this per-concern file, so the
// capture tests read from here.
func mitmWireConcernTestPath(concernRoot string) string {
	return filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernProviderMITMWire))
}

// newMITMCaptureTestHandler returns a gklog.FileJSON handler writing every
// record to the per-concern MITM wire file under concernRoot. gklog.FileJSON
// wraps a NewLockedWriteCloser that takes a blocking cross-process flock per
// write, so concurrent writers serialize without dropping records. The proxy
// test logger is dedicated, so every record it emits is a MITM wire leg and no
// sink filtering is needed.
func newMITMCaptureTestHandler(t *testing.T, concernRoot string) slog.Handler {
	t.Helper()
	path := mitmWireConcernTestPath(concernRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir MITM wire concern dir: %v", err)
	}
	return gklog.FileJSON(path, slog.LevelDebug, gklog.RotationConfig{})
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
