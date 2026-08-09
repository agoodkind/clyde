//go:build live

package live

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/sandbox"

	_ "goodkind.io/clyde/internal/providers/claude/mitmcontrib"
	_ "goodkind.io/clyde/internal/providers/codex/mitmcontrib"
)

func TestLiveMITMIdentityCaptureSandbox(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		const sessionID = "b3e8f88e-3fe8-421f-9cb0-e327103d0a4e"
		requestID := uniqueProbeID(t, "claude-identity")
		assertLiveMITMIdentityCapture(t, identityCaptureExpectation{
			path:                 "/v1/messages",
			requestID:            requestID,
			wantProvider:         "claude",
			wantSessionID:        sessionID,
			wantConversationID:   "claude:" + sessionID,
			wantConversationSource: "header",
			headers: map[string]string{
				"X-Claude-Code-Session-Id": sessionID,
				"X-Client-Request-Id":      requestID,
			},
		})
	})
	t.Run("codex", func(t *testing.T) {
		const threadID = "019fe7c5-3b02-7140-8bb7-a7e7fadeb1e2"
		requestID := uniqueProbeID(t, "codex-identity")
		assertLiveMITMIdentityCapture(t, identityCaptureExpectation{
			path:                 "/backend-api/codex/responses",
			requestID:            requestID,
			wantProvider:         "codex",
			wantSessionID:        threadID,
			wantConversationID:   "codex:" + threadID,
			wantConversationSource: "header",
			headers: map[string]string{
				"Session-Id":          threadID,
				"Thread-Id":           threadID,
				"X-Client-Request-Id": requestID,
			},
		})
	})
}

type identityCaptureExpectation struct {
	path                   string
	requestID              string
	wantProvider           string
	wantSessionID          string
	wantConversationID     string
	wantConversationSource string
	headers                map[string]string
}

func assertLiveMITMIdentityCapture(t *testing.T, expect identityCaptureExpectation) {
	t.Helper()
	roots, err := sandbox.NewRoots()
	if err != nil {
		t.Fatalf("sandbox roots: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(roots.Base) })

	captureDir := filepath.Join(roots.State, "mitm")
	if err := os.MkdirAll(captureDir, 0o700); err != nil {
		t.Fatalf("mkdir capture dir: %v", err)
	}
	dbPath := filepath.Join(captureDir, "capture.db")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy, err := mitm.NewLiveIdentityCaptureProxy(captureDir, dbPath, &http.Client{
		Transport: &rewriteUpstreamTransport{target: upstream},
	})
	if err != nil {
		t.Fatalf("new live identity proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = proxy.Store().Close(context.Background(), "live identity test cleanup")
	})

	req := httptest.NewRequest(http.MethodPost, "http://clyde.test"+expect.path, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("content-type", "application/json")
	for key, value := range expect.headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	proxy.Handle(recorder, req)
	resp := recorder.Result()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d want %d body = %s", resp.StatusCode, http.StatusOK, body)
	}

	if err := mitm.WaitForRequestRow(dbPath, expect.requestID, 10*time.Second); err != nil {
		t.Fatalf("wait for capture row: %v", err)
	}

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var (
		gotProvider           string
		gotSessionID          string
		gotConversationID     string
		gotConversationSource string
	)
	row := db.QueryRow(`SELECT provider, session_id, conversation_id, conversation_source FROM requests WHERE request_id=?`, expect.requestID)
	if err := row.Scan(&gotProvider, &gotSessionID, &gotConversationID, &gotConversationSource); err != nil {
		t.Fatalf("scan identity row: %v", err)
	}
	if gotProvider != expect.wantProvider {
		t.Fatalf("provider = %q want %q", gotProvider, expect.wantProvider)
	}
	if gotSessionID != expect.wantSessionID {
		t.Fatalf("session_id = %q want %q", gotSessionID, expect.wantSessionID)
	}
	if gotConversationID != expect.wantConversationID {
		t.Fatalf("conversation_id = %q want %q", gotConversationID, expect.wantConversationID)
	}
	if gotConversationSource != expect.wantConversationSource {
		t.Fatalf("conversation_source = %q want %q", gotConversationSource, expect.wantConversationSource)
	}
}

type rewriteUpstreamTransport struct {
	target *httptest.Server
	inner  http.RoundTripper
}

func (transport *rewriteUpstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = transport.target.Listener.Addr().String()
	inner := transport.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	return inner.RoundTrip(clone)
}
