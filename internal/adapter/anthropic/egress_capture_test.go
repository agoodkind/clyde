package anthropic

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/mitm/capture"
)

// TestAdapterEgressRecordedToCaptureStore drives one outbound /v1/messages
// exchange through the egress observers and asserts a capture.db row tagged
// client="adapter.anthropic" with the full request and response bodies. The
// response body streams through the tee, so the assertion also confirms the
// tee passes bytes through unchanged.
func TestAdapterEgressRecordedToCaptureStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("capture.Open: %v", err)
	}

	client := New(nil, nil, Config{CaptureStore: store})

	reqBody := []byte(`{"model":"claude-opus-4-8","messages":[]}`)
	respBody := []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {}\n\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"Request-Id":   []string{"req_upstream_1"},
		},
		Body: io.NopCloser(bytes.NewReader(respBody)),
	}
	ex := egressExchange{
		method:  http.MethodPost,
		host:    "api.anthropic.com",
		path:    "/v1/messages",
		reqType: "application/json",
		reqHeaders: http.Header{
			"Authorization": []string{"Bearer test-token"},
			"Content-Type":  []string{"application/json"},
		},
		reqBody: reqBody,
		started: clock.Now(),
	}
	base := responseEvent{Subcomponent: "anthropic", Model: "claude-opus-4-8", Status: http.StatusOK}

	client.attachEgressObservers(context.Background(), resp, base, ex)

	streamed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read tee body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close tee body: %v", err)
	}
	if !bytes.Equal(streamed, respBody) {
		t.Fatalf("tee altered streamed body: got %q want %q", streamed, respBody)
	}
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier: %v", err)
	}
	defer func() { _ = db.Close() }()

	var (
		clientTag, host, method, path, upstreamRequestID, respType string
		status                                                     int
	)
	row := db.QueryRow(`SELECT client, host, method, path, status, upstream_request_id, resp_content_type FROM requests WHERE client='adapter.anthropic'`)
	if err := row.Scan(&clientTag, &host, &method, &path, &status, &upstreamRequestID, &respType); err != nil {
		t.Fatalf("scan requests row: %v", err)
	}
	if clientTag != "adapter.anthropic" {
		t.Fatalf("client = %q, want adapter.anthropic", clientTag)
	}
	if host != "api.anthropic.com" || method != http.MethodPost || path != "/v1/messages" {
		t.Fatalf("host/method/path = %q/%q/%q", host, method, path)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if upstreamRequestID != "req_upstream_1" {
		t.Fatalf("upstream_request_id = %q, want req_upstream_1", upstreamRequestID)
	}
	if respType != "text/event-stream" {
		t.Fatalf("resp_content_type = %q, want text/event-stream", respType)
	}

	var storedReq, storedResp []byte
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE client='adapter.anthropic') AND which='request'`).Scan(&storedReq); err != nil {
		t.Fatalf("scan request body: %v", err)
	}
	if !bytes.Equal(storedReq, reqBody) {
		t.Fatalf("stored request body = %q, want %q", storedReq, reqBody)
	}
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE client='adapter.anthropic') AND which='response'`).Scan(&storedResp); err != nil {
		t.Fatalf("scan response body: %v", err)
	}
	if !bytes.Equal(storedResp, respBody) {
		t.Fatalf("stored response body = %q, want %q", storedResp, respBody)
	}
}

// TestAdapterEgressNilStoreLeavesBodyUntouched confirms that with no capture
// store and wire capture off, the response body is passed through without a tee
// wrapper, so a daemon with MITM disabled records nothing and stays correct.
func TestAdapterEgressNilStoreLeavesBodyUntouched(t *testing.T) {
	client := New(nil, nil, Config{})
	respBody := []byte("hello")
	original := io.NopCloser(bytes.NewReader(respBody))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: original}

	client.attachEgressObservers(context.Background(), resp, responseEvent{Status: http.StatusOK}, egressExchange{started: clock.Now()})

	if resp.Body != original {
		t.Fatalf("resp.Body was wrapped despite nil store and wire capture off")
	}
}
