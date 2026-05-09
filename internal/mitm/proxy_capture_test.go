package mitm

import (
	"bytes"
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
	proxy := newHTTPProxyForCaptureTest(t, captureDir, "raw", upstream)
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
	if len(records) != 2 {
		t.Fatalf("records = %d want 2: %#v", len(records), records)
	}

	requestBodyRef := rawBodyRefFromRecord(t, records[0], "request_body")
	responseBodyRef := rawBodyRefFromRecord(t, records[1], "response_body")
	if requestBodyRef["raw_path"] == "" || responseBodyRef["raw_path"] == "" {
		t.Fatalf("missing raw sidecar paths: request=%#v response=%#v", requestBodyRef, responseBodyRef)
	}
	if _, ok := records[0]["request_body"].(string); ok {
		t.Fatalf("request_body is raw string, want bounded metadata: %#v", records[0]["request_body"])
	}
	if _, ok := records[1]["response_body"].(string); ok {
		t.Fatalf("response_body is raw string, want bounded metadata: %#v", records[1]["response_body"])
	}
	assertFileMode(t, requestBodyRef["raw_path"].(string), rawCaptureFileMode)
	assertFileMode(t, responseBodyRef["raw_path"].(string), rawCaptureFileMode)
	if !bytes.Equal(readFile(t, requestBodyRef["raw_path"].(string)), requestBody) {
		t.Fatalf("request raw sidecar does not match original body")
	}
	if !bytes.Equal(readFile(t, responseBodyRef["raw_path"].(string)), responseBody) {
		t.Fatalf("response raw sidecar does not match original body")
	}
	if records[0]["body_len"].(float64) != float64(len(requestBody)) {
		t.Fatalf("request body_len = %#v want %d", records[0]["body_len"], len(requestBody))
	}
	if records[1]["body_len"].(float64) != float64(len(responseBody)) {
		t.Fatalf("response body_len = %#v want %d", records[1]["body_len"], len(responseBody))
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

	proxy := newHTTPProxyForCaptureTest(t, captureDir, "summary", upstream)
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

func newHTTPProxyForCaptureTest(t *testing.T, captureDir string, bodyMode string, upstream *httptest.Server) *Proxy {
	t.Helper()
	oldOpenAIUpstream := openAIUpstream
	openAIUpstream = upstream.URL
	t.Cleanup(func() {
		openAIUpstream = oldOpenAIUpstream
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := &Proxy{
		log:                   logger,
		client:                upstream.Client(),
		dialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:                sync.Mutex{},
		ca:                    nil,
		cursorTLSClientConfig: nil,
		rawCaptureSeq:         atomic.Uint64{},
		Tunnels:               newTestTunnelRegistry(),
		captureWriters:        newCaptureWriterCache(logger),
		mu:                    sync.RWMutex{},
		cfg:                   config.MITMConfig{CaptureDir: captureDir, BodyMode: bodyMode},
		base:                  "http://[::1]",
		server:                nil,
	}
	t.Cleanup(proxy.closeCaptureWriters)
	return proxy
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
