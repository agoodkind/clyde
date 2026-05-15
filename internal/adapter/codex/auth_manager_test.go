package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// jwtWithExpiryForTest constructs a minimal JWT-shaped string whose
// payload segment carries the given exp claim (Unix seconds). Header
// and signature are placeholders the manager does not inspect.
func jwtWithExpiryForTest(t *testing.T, exp int64) string {
	t.Helper()
	claims, err := json.Marshal(map[string]int64{"exp": exp})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	return "header." + payload + ".sig"
}

func writeAuthFileForTest(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return path
}

func readAuthFileForTest(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("parse auth file: %v", err)
	}
	return top
}

func TestAuthManagerForceRefreshSendsCorrectRequestAndPersistsResponse(t *testing.T) {
	t.Parallel()

	freshAccess := jwtWithExpiryForTest(t, time.Now().Add(1*time.Hour).Unix())
	var captured atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		captured.Store(body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  freshAccess,
			"refresh_token": "rt-rotated",
			"id_token":      "id-new",
		})
	}))
	defer server.Close()

	staleAccess := jwtWithExpiryForTest(t, time.Now().Add(-1*time.Hour).Unix())
	path := writeAuthFileForTest(t, `{
  "auth_mode": "ChatgptAuthTokens",
  "tokens": {
    "access_token": "`+staleAccess+`",
    "refresh_token": "rt-original",
    "account_id": "acct-1"
  },
  "last_refresh": "2026-01-01T00:00:00Z"
}`)

	mgr := NewAuthManager(path, AuthManagerOptions{
		HTTPClient: server.Client(),
		Log:        nil,
		Now:        nil,
		RefreshURL: server.URL,
	})

	got, err := mgr.ForceRefresh(context.Background())
	if err != nil {
		t.Fatalf("ForceRefresh err=%v", err)
	}
	if got != freshAccess {
		t.Fatalf("returned token=%q want %q", got, freshAccess)
	}

	capturedBody, _ := captured.Load().(map[string]string)
	if capturedBody["client_id"] != codexRefreshClientID {
		t.Errorf("client_id=%q want %q", capturedBody["client_id"], codexRefreshClientID)
	}
	if capturedBody["grant_type"] != "refresh_token" {
		t.Errorf("grant_type=%q want refresh_token", capturedBody["grant_type"])
	}
	if capturedBody["refresh_token"] != "rt-original" {
		t.Errorf("refresh_token=%q want rt-original", capturedBody["refresh_token"])
	}

	top := readAuthFileForTest(t, path)
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(top["tokens"], &tokens); err != nil {
		t.Fatalf("parse tokens: %v", err)
	}
	var newAccess, newRefresh, newAccount string
	_ = json.Unmarshal(tokens["access_token"], &newAccess)
	_ = json.Unmarshal(tokens["refresh_token"], &newRefresh)
	_ = json.Unmarshal(tokens["account_id"], &newAccount)
	if newAccess != freshAccess {
		t.Errorf("persisted access_token=%q want %q", newAccess, freshAccess)
	}
	if newRefresh != "rt-rotated" {
		t.Errorf("persisted refresh_token=%q want rt-rotated", newRefresh)
	}
	if newAccount != "acct-1" {
		t.Errorf("account_id=%q want acct-1 (preserved across round-trip)", newAccount)
	}
}

func TestAuthManagerForceRefreshClassifiesRefreshTokenExpiredAsPermanent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_expired","message":"refresh token has expired"}}`))
	}))
	defer server.Close()

	staleAccess := jwtWithExpiryForTest(t, time.Now().Add(-1*time.Hour).Unix())
	path := writeAuthFileForTest(t, `{
  "tokens": {
    "access_token": "`+staleAccess+`",
    "refresh_token": "rt-dead"
  }
}`)

	mgr := NewAuthManager(path, AuthManagerOptions{
		HTTPClient: server.Client(),
		Log:        nil,
		Now:        nil,
		RefreshURL: server.URL,
	})

	_, err := mgr.ForceRefresh(context.Background())
	if err == nil {
		t.Fatalf("ForceRefresh err=nil want error")
	}
	if !IsPermanentRefreshFailure(err) {
		t.Fatalf("err=%v should be permanent", err)
	}
	var refreshErr *AuthRefreshError
	if !errors.As(err, &refreshErr) {
		t.Fatalf("err=%v should unwrap to *AuthRefreshError", err)
	}
	if refreshErr.Code != "refresh_token_expired" {
		t.Errorf("code=%q want refresh_token_expired", refreshErr.Code)
	}
	if refreshErr.Status != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", refreshErr.Status)
	}
}

func TestAuthManagerForceRefreshTreatsServerErrorAsTransient(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer server.Close()

	path := writeAuthFileForTest(t, `{"tokens":{"access_token":"`+jwtWithExpiryForTest(t, time.Now().Add(-1*time.Hour).Unix())+`","refresh_token":"rt"}}`)
	mgr := NewAuthManager(path, AuthManagerOptions{
		HTTPClient: server.Client(),
		Log:        nil,
		Now:        nil,
		RefreshURL: server.URL,
	})

	_, err := mgr.ForceRefresh(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	if IsPermanentRefreshFailure(err) {
		t.Fatalf("5xx must classify as transient, got permanent: %v", err)
	}
}

func TestAuthManagerRoundTripPreservesUnknownFields(t *testing.T) {
	t.Parallel()

	freshAccess := jwtWithExpiryForTest(t, time.Now().Add(1*time.Hour).Unix())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  freshAccess,
			"refresh_token": "rt-new",
		})
	}))
	defer server.Close()

	staleAccess := jwtWithExpiryForTest(t, time.Now().Add(-1*time.Hour).Unix())
	path := writeAuthFileForTest(t, `{
  "auth_mode": "ChatgptAuthTokens",
  "openai_api_key": "preserved-extra-key",
  "tokens": {
    "access_token": "`+staleAccess+`",
    "refresh_token": "rt-original",
    "account_id": "acct-1",
    "id_token": {"sub": "user-123", "iss": "openai"},
    "extra_codex_field": "preserve-me"
  }
}`)

	mgr := NewAuthManager(path, AuthManagerOptions{
		HTTPClient: server.Client(),
		Log:        nil,
		Now:        nil,
		RefreshURL: server.URL,
	})

	if _, err := mgr.ForceRefresh(context.Background()); err != nil {
		t.Fatalf("ForceRefresh err=%v", err)
	}

	top := readAuthFileForTest(t, path)
	var authMode string
	if err := json.Unmarshal(top["auth_mode"], &authMode); err != nil || authMode != "ChatgptAuthTokens" {
		t.Errorf("auth_mode lost: %q (err=%v)", authMode, err)
	}
	var openaiAPIKey string
	if err := json.Unmarshal(top["openai_api_key"], &openaiAPIKey); err != nil || openaiAPIKey != "preserved-extra-key" {
		t.Errorf("openai_api_key lost: %q (err=%v)", openaiAPIKey, err)
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(top["tokens"], &tokens); err != nil {
		t.Fatalf("parse tokens: %v", err)
	}
	var extra string
	if err := json.Unmarshal(tokens["extra_codex_field"], &extra); err != nil || extra != "preserve-me" {
		t.Errorf("extra_codex_field lost: %q (err=%v)", extra, err)
	}
	idTokenRaw, ok := tokens["id_token"]
	if !ok {
		t.Fatalf("id_token lost")
	}
	// Structured id_token should round-trip as-is (raw object).
	if !strings.Contains(string(idTokenRaw), "user-123") {
		t.Errorf("id_token object lost; got %s", string(idTokenRaw))
	}
}

func TestExtractCodexRefreshErrorCodeReadsTopLevelAndNested(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "top_level_code", body: `{"code":"refresh_token_reused"}`, want: "refresh_token_reused"},
		{name: "nested_error_code", body: `{"error":{"code":"refresh_token_invalidated"}}`, want: "refresh_token_invalidated"},
		{name: "error_as_string", body: `{"error":"invalid_grant"}`, want: "invalid_grant"},
		{name: "empty", body: `{}`, want: ""},
		{name: "garbage", body: `not-json`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractCodexRefreshErrorCode([]byte(tc.body))
			if got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}
