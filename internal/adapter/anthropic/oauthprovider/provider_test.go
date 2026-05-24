package oauthprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

// realisticCredentialsBlob returns a Claude .credentials.json document body
// shaped like the one Claude Code writes.
func realisticCredentialsBlob(t *testing.T) []byte {
	t.Helper()
	tokens := &oauthcredentials.Tokens{
		AccessToken:      "fake-oat-access-secret",  //gitleaks:allow test fixture, not a real token
		RefreshToken:     "fake-ort-refresh-secret", //gitleaks:allow test fixture, not a real token
		ExpiresAt:        time.UnixMilli(1_900_000_000_000).UnixMilli(),
		Scopes:           []string{"user:inference", "user:profile"},
		SubscriptionType: "pro",
		RateLimitTier:    "default",
	}
	raw, err := json.Marshal(oauthcredentials.Document{ClaudeAIOauth: tokens})
	if err != nil {
		t.Fatalf("encode realistic blob: %v", err)
	}
	return raw
}

func TestParseEncodeStoredRoundTrip(t *testing.T) {
	p := New(config.AdapterOAuth{}, t.TempDir())
	raw := realisticCredentialsBlob(t)

	creds, err := p.ParseStored(raw)
	if err != nil {
		t.Fatalf("ParseStored: %v", err)
	}
	if creds.AccessToken != "fake-oat-access-secret" {
		t.Fatalf("AccessToken = %q, want fake-oat-access-secret", creds.AccessToken)
	}
	if creds.RefreshToken != "fake-ort-refresh-secret" {
		t.Fatalf("RefreshToken = %q, want fake-ort-refresh-secret", creds.RefreshToken)
	}
	if creds.ExpiresAt.UnixMilli() != 1_900_000_000_000 {
		t.Fatalf("ExpiresAt = %d, want 1900000000000", creds.ExpiresAt.UnixMilli())
	}
	if creds.Fingerprint == "" {
		t.Fatal("Fingerprint is empty")
	}

	encoded, err := p.EncodeStored(creds)
	if err != nil {
		t.Fatalf("EncodeStored: %v", err)
	}
	reparsed, err := p.ParseStored(encoded)
	if err != nil {
		t.Fatalf("ParseStored(encoded): %v", err)
	}
	if reparsed.Fingerprint != creds.Fingerprint {
		t.Fatalf("round-trip fingerprint drift: %q -> %q", creds.Fingerprint, reparsed.Fingerprint)
	}

	// The provider-only fields (scopes, subscriptionType, rateLimitTier) must
	// survive the round trip via Raw so the fingerprint is stable.
	var doc struct {
		ClaudeAIOauth oauthcredentials.Tokens `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatalf("unmarshal encoded: %v", err)
	}
	if doc.ClaudeAIOauth.SubscriptionType != "pro" {
		t.Fatalf("subscriptionType lost: got %q", doc.ClaudeAIOauth.SubscriptionType)
	}
	if strings.Join(doc.ClaudeAIOauth.Scopes, " ") != "user:inference user:profile" {
		t.Fatalf("scopes lost: got %v", doc.ClaudeAIOauth.Scopes)
	}
}

func TestParseStoredRejectsMissingEntry(t *testing.T) {
	p := New(config.AdapterOAuth{}, t.TempDir())
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "empty object", raw: []byte(`{}`)},
		{name: "malformed json", raw: []byte(`{not json`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.ParseStored(tc.raw); err == nil {
				t.Fatalf("ParseStored(%q) returned nil error", tc.raw)
			}
		})
	}
}

func TestMirrorSources(t *testing.T) {
	cfg := config.AdapterOAuth{KeychainService: "Claude Code-credentials"}
	credentialsDir := "/home/example/.claude"
	p := New(cfg, credentialsDir)

	sources, err := p.MirrorSources(context.Background())
	if err != nil {
		t.Fatalf("MirrorSources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want 2: %+v", len(sources), sources)
	}
	wantFile := provider.MirrorSource{
		Kind:     provider.MirrorSourceKindFile,
		Location: filepath.Join(credentialsDir, ".credentials.json"),
	}
	if sources[0] != wantFile {
		t.Fatalf("file source = %+v, want %+v", sources[0], wantFile)
	}
	wantKeychain := provider.MirrorSource{
		Kind:     provider.MirrorSourceKindKeychain,
		Location: "Claude Code-credentials",
	}
	if sources[1] != wantKeychain {
		t.Fatalf("keychain source = %+v, want %+v", sources[1], wantKeychain)
	}
}

func TestReadMirrorFileSource(t *testing.T) {
	credentialsDir := t.TempDir()
	p := New(config.AdapterOAuth{}, credentialsDir)
	credsPath := filepath.Join(credentialsDir, ".credentials.json")
	if err := os.WriteFile(credsPath, realisticCredentialsBlob(t), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	src := provider.MirrorSource{Kind: provider.MirrorSourceKindFile, Location: credsPath}
	cred, present, err := p.ReadMirror(context.Background(), src)
	if err != nil {
		t.Fatalf("ReadMirror: %v", err)
	}
	if !present {
		t.Fatal("present = false, want true")
	}
	if cred.AccessToken != "fake-oat-access-secret" {
		t.Fatalf("AccessToken = %q, want fake-oat-access-secret", cred.AccessToken)
	}
	if cred.Fingerprint == "" {
		t.Fatal("Fingerprint is empty")
	}
}

func TestReadMirrorAbsentFileSource(t *testing.T) {
	credentialsDir := t.TempDir()
	p := New(config.AdapterOAuth{}, credentialsDir)

	src := provider.MirrorSource{
		Kind:     provider.MirrorSourceKindFile,
		Location: filepath.Join(credentialsDir, ".credentials.json"),
	}
	cred, present, err := p.ReadMirror(context.Background(), src)
	if err != nil {
		t.Fatalf("ReadMirror: %v", err)
	}
	if present {
		t.Fatal("present = true, want false for a missing file")
	}
	if cred.AccessToken != "" {
		t.Fatalf("AccessToken = %q, want empty", cred.AccessToken)
	}
}

func TestReadMirrorUnknownKind(t *testing.T) {
	p := New(config.AdapterOAuth{}, t.TempDir())
	src := provider.MirrorSource{Kind: "bogus", Location: "x"}
	if _, _, err := p.ReadMirror(context.Background(), src); err == nil {
		t.Fatal("ReadMirror(bogus kind) returned nil error")
	}
}

// jwtWithSub builds a three-segment JWT whose payload carries the given sub
// claim. The signature segment is filler since AccountUUIDFromAccessToken only
// reads the payload.
func jwtWithSub(t *testing.T, sub string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadJSON, err := json.Marshal(struct {
		Sub string `json:"sub"`
	}{Sub: sub})
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signature := base64.RawURLEncoding.EncodeToString([]byte("signature"))
	return header + "." + payload + "." + signature
}

func TestAccountID(t *testing.T) {
	p := New(config.AdapterOAuth{}, t.TempDir())

	t.Run("jwt token", func(t *testing.T) {
		token := jwtWithSub(t, "acct-12345")
		account, err := p.AccountID(token)
		if err != nil {
			t.Fatalf("AccountID: %v", err)
		}
		if account != provider.AccountID("acct-12345") {
			t.Fatalf("AccountID = %q, want acct-12345", account)
		}
	})

	t.Run("opaque token yields empty", func(t *testing.T) {
		account, err := p.AccountID("fake-oat-opaque")
		if err != nil {
			t.Fatalf("AccountID: %v", err)
		}
		if account != "" {
			t.Fatalf("AccountID = %q, want empty for opaque token", account)
		}
	})
}

func TestRefreshSuccess(t *testing.T) {
	capturedBody := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fake-oat-new","refresh_token":"fake-ort-new","expires_in":3600,"scope":"user:inference user:profile"}`))
	}))
	defer server.Close()

	cfg := config.AdapterOAuth{
		TokenURL: server.URL,
		ClientID: "client-abc",
		Scopes:   []string{"user:inference", "user:profile"},
	}
	p := New(cfg, t.TempDir())

	current := provider.Credentials{
		AccessToken:  "fake-oat-old",
		RefreshToken: "fake-ort-old",
		ExpiresAt:    time.UnixMilli(1_000),
	}
	before := time.Now().UnixMilli()
	refreshed, err := p.Refresh(context.Background(), current)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.AccessToken != "fake-oat-new" {
		t.Fatalf("AccessToken = %q, want fake-oat-new", refreshed.AccessToken)
	}
	if refreshed.RefreshToken != "fake-ort-new" {
		t.Fatalf("RefreshToken = %q, want fake-ort-new", refreshed.RefreshToken)
	}
	if refreshed.ExpiresAt.UnixMilli() < before+3600*1000 {
		t.Fatalf("ExpiresAt = %d not at least ~1h ahead of %d", refreshed.ExpiresAt.UnixMilli(), before)
	}
	if capturedBody["grant_type"] != "refresh_token" {
		t.Fatalf("grant_type = %q, want refresh_token", capturedBody["grant_type"])
	}
	if capturedBody["refresh_token"] != "fake-ort-old" {
		t.Fatalf("refresh_token = %q, want fake-ort-old", capturedBody["refresh_token"])
	}
	if capturedBody["client_id"] != "client-abc" {
		t.Fatalf("client_id = %q, want client-abc", capturedBody["client_id"])
	}
	if capturedBody["scope"] != "user:inference user:profile" {
		t.Fatalf("scope = %q, want joined scopes", capturedBody["scope"])
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token rotated"}`))
	}))
	defer server.Close()

	p := New(config.AdapterOAuth{TokenURL: server.URL, ClientID: "c", Scopes: []string{"s"}}, t.TempDir())
	_, err := p.Refresh(context.Background(), provider.Credentials{RefreshToken: "dead"})
	if err == nil {
		t.Fatal("Refresh returned nil error on invalid_grant")
	}
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("error %v does not wrap ErrInvalidGrant", err)
	}
}

func TestRefreshRequiresRefreshToken(t *testing.T) {
	p := New(config.AdapterOAuth{}, t.TempDir())
	if _, err := p.Refresh(context.Background(), provider.Credentials{}); err == nil {
		t.Fatal("Refresh returned nil error without a refresh token")
	}
}

func TestSpawnLoginScrapesURL(t *testing.T) {
	const wantURL = "https://claude.ai/oauth/authorize?code_challenge=abc&state=xyz"
	restore := stubClaudeBinary(t, fakeClaudeScript(wantURL, 0, true))
	defer restore()

	p := New(config.AdapterOAuth{}, t.TempDir())
	session, err := p.SpawnLogin(context.Background(), provider.LoginOptions{Email: "user@example.com"})
	if err != nil {
		t.Fatalf("SpawnLogin: %v", err)
	}
	if session.AuthorizeURL != wantURL {
		t.Fatalf("AuthorizeURL = %q, want %q", session.AuthorizeURL, wantURL)
	}
	if session.Handle == "" {
		t.Fatal("Handle is empty")
	}
	if session.ScratchDir == "" {
		t.Fatal("ScratchDir is empty")
	}
	if _, statErr := os.Stat(session.ScratchDir); statErr != nil {
		t.Fatalf("ScratchDir not created: %v", statErr)
	}

	// CompleteLogin should read the credentials the fake wrote and clean up.
	creds, err := p.CompleteLogin(context.Background(), session)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if creds.AccessToken != "fake-oat-from-login" {
		t.Fatalf("AccessToken = %q, want fake-oat-from-login", creds.AccessToken)
	}
	if _, statErr := os.Stat(session.ScratchDir); !os.IsNotExist(statErr) {
		t.Fatalf("ScratchDir not removed after CompleteLogin: stat err = %v", statErr)
	}
}

func TestCompleteLoginNonZeroExit(t *testing.T) {
	restore := stubClaudeBinary(t, fakeClaudeScript("https://claude.ai/oauth/authorize?x=1", 7, false))
	defer restore()

	p := New(config.AdapterOAuth{}, t.TempDir())
	session, err := p.SpawnLogin(context.Background(), provider.LoginOptions{})
	if err != nil {
		t.Fatalf("SpawnLogin: %v", err)
	}
	_, err = p.CompleteLogin(context.Background(), session)
	if err == nil {
		t.Fatal("CompleteLogin returned nil error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "exit 7") {
		t.Fatalf("error %q missing exit code", err.Error())
	}
}

// stubClaudeBinary writes a fake `claude` executable into a temp dir, prepends
// that dir to PATH, and overrides commandContext so the child runs the fake
// binary by its absolute path even after SpawnLogin strips browser-opener dirs
// from the child PATH. It returns a restore func.
func stubClaudeBinary(t *testing.T, script string) func() {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "claude")
	if err := os.WriteFile(binPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	origCommandContext := commandContext
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "claude" {
			name = binPath
		}
		return exec.CommandContext(ctx, name, args...)
	}
	return func() { commandContext = origCommandContext }
}

// fakeClaudeScript builds a POSIX shell script that prints an authorize URL,
// optionally writes a credentials file under $HOME/.claude, and exits with the
// given code.
func fakeClaudeScript(authorizeURL string, exitCode int, writeCreds bool) string {
	var builder strings.Builder
	builder.WriteString("#!/bin/sh\n")
	builder.WriteString("echo 'Visit the following URL to authorize:'\n")
	builder.WriteString("echo '" + authorizeURL + "'\n")
	if writeCreds {
		builder.WriteString("mkdir -p \"$HOME/.claude\"\n")
		builder.WriteString("cat > \"$HOME/.claude/.credentials.json\" <<'JSON'\n")
		builder.WriteString(`{"claudeAiOauth":{"accessToken":"fake-oat-from-login","refreshToken":"fake-ort-from-login","expiresAt":1900000000000,"scopes":["user:inference"]}}` + "\n")
		builder.WriteString("JSON\n")
	}
	if exitCode != 0 {
		builder.WriteString("exit " + strconv.Itoa(exitCode) + "\n")
	}
	return builder.String()
}
