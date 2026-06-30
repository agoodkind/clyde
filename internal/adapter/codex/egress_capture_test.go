package codex

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

func openCodexCaptureStore(t *testing.T) (*capture.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("capture.Open: %v", err)
	}
	return store, dbPath
}

func queryCodexRow(t *testing.T, dbPath, which string) []byte {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier: %v", err)
	}
	defer func() { _ = db.Close() }()
	var data []byte
	row := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE client='adapter.codex') AND which=?`, which)
	if err := row.Scan(&data); err != nil {
		t.Fatalf("scan %s body: %v", which, err)
	}
	return data
}

// TestCodexHTTPEgressRecorded drives one Codex HTTP Responses exchange through
// the recorder and asserts a capture.db row tagged client="adapter.codex" with
// the full request and response bodies and the derived host and path.
func TestCodexHTTPEgressRecorded(t *testing.T) {
	store, dbPath := openCodexCaptureStore(t)

	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"req_cx_1"},
		},
	}
	reqBody := []byte(`{"model":"gpt-5.4","input":[]}`)
	respBody := []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")

	recordCodexHTTPEgress(store, correlation.Context{}, req, resp, reqBody, respBody, "conv-1", clock.Now())
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier: %v", err)
	}
	var host, path, upstreamRequestID string
	var status int
	row := db.QueryRow(`SELECT host, path, status, upstream_request_id FROM requests WHERE client='adapter.codex'`)
	if err := row.Scan(&host, &path, &status, &upstreamRequestID); err != nil {
		t.Fatalf("scan requests row: %v", err)
	}
	_ = db.Close()
	if host != "chatgpt.com" || path != "/backend-api/codex/responses" {
		t.Fatalf("host/path = %q/%q", host, path)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if upstreamRequestID != "req_cx_1" {
		t.Fatalf("upstream_request_id = %q, want req_cx_1", upstreamRequestID)
	}
	if got := queryCodexRow(t, dbPath, "request"); !bytes.Equal(got, reqBody) {
		t.Fatalf("request body = %q, want %q", got, reqBody)
	}
	if got := queryCodexRow(t, dbPath, "response"); !bytes.Equal(got, respBody) {
		t.Fatalf("response body = %q, want %q", got, respBody)
	}
}

// TestCodexWebsocketFullFrameRecorded confirms the websocket recorder saves the
// whole exchange in one row: the outbound frame as the request body and every
// inbound frame newline-delimited as the response body.
func TestCodexWebsocketFullFrameRecorded(t *testing.T) {
	store, dbPath := openCodexCaptureStore(t)

	outbound := []byte(`{"type":"response.create","model":"gpt-5.4"}`)
	frame1 := []byte(`{"type":"response.output_item.added"}`)
	frame2 := []byte(`{"type":"response.completed"}`)

	rec := newWsFrameRecorder(&wsEgressCapture{
		store:     store,
		corr:      correlation.Context{},
		url:       "wss://chatgpt.com/backend-api/codex/responses",
		outbound:  outbound,
		sessionID: "conv-ws-1",
		started:   clock.Now(),
	})
	rec.add(frame1)
	rec.add(frame2)
	rec.record(http.StatusOK)
	// A second record call must be a no-op (the loop's defer fires after an
	// explicit error-path record).
	rec.record(0)

	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier: %v", err)
	}
	var count, status int
	var host string
	if err := db.QueryRow(`SELECT count(*), max(status), max(host) FROM requests WHERE client='adapter.codex'`).Scan(&count, &status, &host); err != nil {
		t.Fatalf("scan requests: %v", err)
	}
	_ = db.Close()
	if count != 1 {
		t.Fatalf("requests count = %d, want 1 (record must be idempotent)", count)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if host != "chatgpt.com" {
		t.Fatalf("host = %q, want chatgpt.com", host)
	}
	if got := queryCodexRow(t, dbPath, "request"); !bytes.Equal(got, outbound) {
		t.Fatalf("request body = %q, want outbound frame %q", got, outbound)
	}
	wantResp := append(append(append([]byte{}, frame1...), '\n'), frame2...)
	if got := queryCodexRow(t, dbPath, "response"); !bytes.Equal(got, wantResp) {
		t.Fatalf("response body = %q, want joined frames %q", got, wantResp)
	}
}
