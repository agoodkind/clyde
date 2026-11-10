package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeOAuth implements OAuthSource for the on-401 retry tests. Token returns
// initialToken on every call. ForceRefresh increments calls and returns the
// configured recovery token plus the configured recovery error.
type fakeOAuth struct {
	initialToken   string
	recoveredToken string
	recoverErr     error
	tokenCalls     atomic.Int64
	recoverCalls   atomic.Int64
}

func (f *fakeOAuth) Token(_ context.Context) (string, error) {
	f.tokenCalls.Add(1)
	return f.initialToken, nil
}

func (f *fakeOAuth) ForceRefresh(_ context.Context) (string, error) {
	f.recoverCalls.Add(1)
	return f.recoveredToken, f.recoverErr
}

// newRetryTestServer builds an httptest server whose handler returns a 401
// for the first request and the result of secondStatus (200 or 401) for
// subsequent requests. Every observed Authorization header is appended to
// authObserved so the test can assert the retry posted with a fresh token.
func newRetryTestServer(t *testing.T, secondStatus int, authObserved *[]string) *httptest.Server {
	t.Helper()
	var requestCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*authObserved = append(*authObserved, r.Header.Get("Authorization"))
		count := requestCount.Add(1)
		if count == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.WriteHeader(secondStatus)
		if secondStatus == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"still unauthorized"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoRetriesOn401WithFreshToken(t *testing.T) {
	t.Parallel()
	var auths []string
	srv := newRetryTestServer(t, http.StatusOK, &auths)

	oauth := &fakeOAuth{
		initialToken:   "stale-token",
		recoveredToken: "fresh-token",
		recoverErr:     nil,
		tokenCalls:     atomic.Int64{},
		recoverCalls:   atomic.Int64{},
	}
	cli := &Client{
		http:         srv.Client(),
		oauth:        oauth,
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:           srv.URL + "/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			BetaHeader:            "REDACTED-OAUTH-BETA",
			UserAgent:             "anthropic-test/0",
			CCVersion:             "1.0.0",
			CCEntrypoint:          "test",
			CaptureStore:          seedTestWireBaseline(t),
		},
	}

	resp, err := cli.do(context.Background(), Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := oauth.recoverCalls.Load(); got != 1 {
		t.Fatalf("recoverCalls = %d, want 1", got)
	}
	if len(auths) != 2 {
		t.Fatalf("server observed %d requests, want 2", len(auths))
	}
	if auths[0] != "Bearer stale-token" {
		t.Fatalf("first auth = %q, want Bearer stale-token", auths[0])
	}
	if auths[1] != "Bearer fresh-token" {
		t.Fatalf("retry auth = %q, want Bearer fresh-token", auths[1])
	}
}

func TestDoFallsThroughOnSecond401(t *testing.T) {
	t.Parallel()
	var auths []string
	srv := newRetryTestServer(t, http.StatusUnauthorized, &auths)

	oauth := &fakeOAuth{
		initialToken:   "stale-token",
		recoveredToken: "fresh-token",
		recoverErr:     nil,
		tokenCalls:     atomic.Int64{},
		recoverCalls:   atomic.Int64{},
	}
	cli := &Client{
		http:         srv.Client(),
		oauth:        oauth,
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:           srv.URL + "/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			BetaHeader:            "REDACTED-OAUTH-BETA",
			UserAgent:             "anthropic-test/0",
			CCVersion:             "1.0.0",
			CCEntrypoint:          "test",
			CaptureStore:          seedTestWireBaseline(t),
		},
	}

	_, err := cli.do(context.Background(), Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	})
	if err == nil {
		t.Fatalf("expected upstream error, got nil")
	}
	// The second 401 must surface as a typed UpstreamError so the existing
	// classifier maps it to upstream_auth_failed downstream.
	ue, ok := AsUpstreamError(err)
	if !ok {
		t.Fatalf("err type = %T, want *UpstreamError", err)
	}
	if ue.Status != http.StatusUnauthorized {
		t.Fatalf("UpstreamError.Status = %d, want 401", ue.Status)
	}
	if got := oauth.recoverCalls.Load(); got != 1 {
		t.Fatalf("recoverCalls = %d, want 1", got)
	}
	// One initial + one retry; no third post.
	if len(auths) != 2 {
		t.Fatalf("server observed %d requests, want 2", len(auths))
	}
}

func TestDoSkipsRetryWhenRecoverErrors(t *testing.T) {
	t.Parallel()
	var auths []string
	// Server returns 401 once. With recover-error the client never posts a
	// second time; the test still allows a 200 second response so a bug that
	// retries would be visible as a 200 result.
	srv := newRetryTestServer(t, http.StatusOK, &auths)

	oauth := &fakeOAuth{
		initialToken:   "stale-token",
		recoveredToken: "stale-token",
		recoverErr:     errors.New("recover failed"),
		tokenCalls:     atomic.Int64{},
		recoverCalls:   atomic.Int64{},
	}
	cli := &Client{
		http:         srv.Client(),
		oauth:        oauth,
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:           srv.URL + "/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			BetaHeader:            "REDACTED-OAUTH-BETA",
			UserAgent:             "anthropic-test/0",
			CCVersion:             "1.0.0",
			CCEntrypoint:          "test",
			CaptureStore:          seedTestWireBaseline(t),
		},
	}

	_, err := cli.do(context.Background(), Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	})
	if err == nil {
		t.Fatalf("expected 401 error to propagate, got nil")
	}
	ue, ok := AsUpstreamError(err)
	if !ok {
		t.Fatalf("err type = %T, want *UpstreamError", err)
	}
	if ue.Status != http.StatusUnauthorized {
		t.Fatalf("UpstreamError.Status = %d, want 401", ue.Status)
	}
	if got := oauth.recoverCalls.Load(); got != 1 {
		t.Fatalf("recoverCalls = %d, want 1", got)
	}
	if len(auths) != 1 {
		t.Fatalf("server observed %d requests, want 1 (no retry)", len(auths))
	}
	if !strings.Contains(ue.Message, "unauthorized") {
		t.Fatalf("UpstreamError.Message = %q, want it to carry the original 401 body text", ue.Message)
	}
}

func TestDoSkipsRetryWhenTokenUnchanged(t *testing.T) {
	t.Parallel()
	var auths []string
	srv := newRetryTestServer(t, http.StatusOK, &auths)

	oauth := &fakeOAuth{
		initialToken:   "stale-token",
		recoveredToken: "stale-token",
		recoverErr:     nil,
		tokenCalls:     atomic.Int64{},
		recoverCalls:   atomic.Int64{},
	}
	cli := &Client{
		http:         srv.Client(),
		oauth:        oauth,
		flavorLoader: newWireFlavorsLoader(),
		cfg: Config{
			MessagesURL:           srv.URL + "/v1/messages",
			OAuthAnthropicVersion: "2023-06-01",
			BetaHeader:            "REDACTED-OAUTH-BETA",
			UserAgent:             "anthropic-test/0",
			CCVersion:             "1.0.0",
			CCEntrypoint:          "test",
			CaptureStore:          seedTestWireBaseline(t),
		},
	}

	_, err := cli.do(context.Background(), Request{
		Model:     "claude-test",
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "x"}}}},
		MaxTokens: 10,
	})
	if err == nil {
		t.Fatalf("expected 401 to propagate when recover returns same token, got nil")
	}
	ue, ok := AsUpstreamError(err)
	if !ok {
		t.Fatalf("err type = %T, want *UpstreamError", err)
	}
	if ue.Status != http.StatusUnauthorized {
		t.Fatalf("UpstreamError.Status = %d, want 401", ue.Status)
	}
	if len(auths) != 1 {
		t.Fatalf("server observed %d requests, want 1 (no retry on unchanged token)", len(auths))
	}
	if !strings.Contains(ue.Message, "unauthorized") {
		t.Fatalf("UpstreamError.Message = %q, want it to carry the original 401 body text", ue.Message)
	}
}
