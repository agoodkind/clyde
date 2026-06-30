package adapter

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // database/sql driver "sqlite3"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

func openIngressTestStore(t *testing.T) (*capture.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{
		DBPath: dbPath, MaxBodyBytes: 0, RetentionMaxAge: 0, RetentionMaxBytes: 0, RetentionInterval: 0, QueueDepth: 0,
	}, nil)
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	return store, dbPath
}

func ingressTestServer(store *capture.Store, enabled bool) *Server {
	return &Server{
		deps: Deps{CaptureStore: store},
		cfg:  config.AdapterConfig{CaptureIngress: enabled},
	}
}

// TestBeginIngressCaptureGate locks in that the wrapper is installed only when
// the capture store is open AND the knob is on, and that it is a true no-op
// (writer untouched) otherwise.
func TestBeginIngressCaptureGate(t *testing.T) {
	store, _ := openIngressTestStore(t)
	t.Cleanup(func() { _ = store.Close(context.Background(), "test") })

	cases := []struct {
		name    string
		store   *capture.Store
		enabled bool
		wantNil bool
	}{
		{name: "store nil", store: nil, enabled: true, wantNil: true},
		{name: "knob off", store: store, enabled: false, wantNil: true},
		{name: "store and knob on", store: store, enabled: true, wantNil: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ingressTestServer(tc.store, tc.enabled)
			base := httptest.NewRecorder()
			hctx := &handlerCtx{Writer: base, Request: nil, Family: adapterRouteOpenAI, RequestID: "r", Correlation: correlation.Context{}}
			capW := s.beginIngressCapture(hctx)
			if (capW == nil) != tc.wantNil {
				t.Fatalf("beginIngressCapture nil=%v, want nil=%v", capW == nil, tc.wantNil)
			}
			if tc.wantNil {
				if hctx.Writer != base {
					t.Fatal("writer was wrapped when capture is off")
				}
				return
			}
			if _, ok := hctx.Writer.(*ingressCaptureWriter); !ok {
				t.Fatalf("hctx.Writer not wrapped, got %T", hctx.Writer)
			}
		})
	}
}

// TestIngressCaptureRecordsResponseAndRedactsAuth drives a streamed response
// through the wrapper and asserts an adapter.ingress capture.db row holds the
// raw request body and the rendered response, with the Authorization header
// dropped.
func TestIngressCaptureRecordsResponseAndRedactsAuth(t *testing.T) {
	store, dbPath := openIngressTestStore(t)
	s := ingressTestServer(store, true)

	base := httptest.NewRecorder()
	hctx := &handlerCtx{Writer: base, Request: nil, Family: adapterRouteOpenAI, RequestID: "req-ing-1", Correlation: correlation.Context{}}
	capW := s.beginIngressCapture(hctx)
	if capW == nil {
		t.Fatal("expected wrapper")
	}

	hctx.Writer.Header().Set("Content-Type", "text/event-stream")
	respChunks := [][]byte{[]byte("data: {\"id\":\"chatcmpl-x\"}\n\n"), []byte("data: [DONE]\n\n")}
	for _, chunk := range respChunks {
		if _, err := hctx.Writer.Write(chunk); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-should-not-store")
	reqBody := []byte(`{"model":"clyde-x","messages":[{"role":"user","content":"hi"}]}`)
	corr := correlation.Context{RequestID: "req-ing-1"}
	s.finishIngressCapture(capW, corr, req, reqBody, clock.Now(), nil)

	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close store: %v", err)
	}

	db := openIngressVerifier(t, dbPath)
	var (
		client, path, respType string
		status                 int
	)
	row := db.QueryRow(`SELECT client, path, status, resp_content_type FROM requests WHERE client='adapter.ingress'`)
	if err := row.Scan(&client, &path, &status, &respType); err != nil {
		t.Fatalf("scan request row: %v", err)
	}
	if path != "/v1/chat/completions" || status != http.StatusOK || respType != "text/event-stream" {
		t.Fatalf("row path=%q status=%d resp_type=%q", path, status, respType)
	}

	gotReq := ingressBody(t, db, "request")
	if string(gotReq) != string(reqBody) {
		t.Fatalf("request body=%q want %q", gotReq, reqBody)
	}
	wantResp := string(respChunks[0]) + string(respChunks[1])
	if gotResp := ingressBody(t, db, "response"); string(gotResp) != wantResp {
		t.Fatalf("response body=%q want %q", gotResp, wantResp)
	}

	var headers string
	if err := db.QueryRow(`SELECT req_headers FROM requests WHERE client='adapter.ingress'`).Scan(&headers); err != nil {
		t.Fatalf("scan req_headers: %v", err)
	}
	if strings.Contains(headers, "secret-should-not-store") {
		t.Fatalf("req_headers leaked Authorization: %q", headers)
	}
}

// TestIngressCaptureErrorStatusFromAdapterError covers a request that errored
// before any write: the wrapper saw no status, so the row carries the returned
// adapterError's HTTP status.
func TestIngressCaptureErrorStatusFromAdapterError(t *testing.T) {
	store, dbPath := openIngressTestStore(t)
	s := ingressTestServer(store, true)

	base := httptest.NewRecorder()
	hctx := &handlerCtx{Writer: base, Request: nil, Family: adapterRouteOpenAI, RequestID: "req-err", Correlation: correlation.Context{}}
	capW := s.beginIngressCapture(hctx)
	if capW == nil {
		t.Fatal("expected wrapper")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	aerr := newAdapterError(adapterErrorInvalidRequest, "bad request")
	s.finishIngressCapture(capW, correlation.Context{RequestID: "req-err"}, req, []byte(`{}`), clock.Now(), aerr)

	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close store: %v", err)
	}

	db := openIngressVerifier(t, dbPath)
	var status int
	if err := db.QueryRow(`SELECT status FROM requests WHERE client='adapter.ingress'`).Scan(&status); err != nil {
		t.Fatalf("scan status: %v", err)
	}
	if status != aerr.HTTPStatus || status == 0 {
		t.Fatalf("error-row status=%d want %d (adapterError HTTPStatus)", status, aerr.HTTPStatus)
	}
}

func openIngressVerifier(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func ingressBody(t *testing.T, db *sql.DB, which string) []byte {
	t.Helper()
	var data []byte
	q := `SELECT data FROM bodies WHERE which=? AND request_row_id=(SELECT id FROM requests WHERE client='adapter.ingress')`
	if err := db.QueryRow(q, which).Scan(&data); err != nil {
		t.Fatalf("scan %s body: %v", which, err)
	}
	return data
}
