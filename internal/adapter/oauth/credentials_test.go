package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

func TestSelectCredentialCandidate_KeychainWinsWhenEquallyUsable(t *testing.T) {
	now := oauthClock.Now()
	keychain := credentialReadResult(oauthcredentials.SourceKeychain, tokenWithExpiry(now.Add(time.Hour), true), now)
	file := credentialReadResult(oauthcredentials.SourceFile, tokenWithExpiry(now.Add(time.Hour), true), now)
	selected, err := selectCredentialCandidate([]oauthcredentials.ReadResult{file, keychain})
	if err != nil {
		t.Fatalf("selectCredentialCandidate error = %v", err)
	}
	if selected.Source != oauthcredentials.SourceKeychain {
		t.Fatalf("source = %q, want keychain", selected.Source)
	}
}

func TestSelectCredentialCandidate_FileWinsWhenKeychainExpiredWithoutRefresh(t *testing.T) {
	now := oauthClock.Now()
	keychain := credentialReadResult(oauthcredentials.SourceKeychain, tokenWithExpiry(now.Add(-time.Hour), false), now)
	file := credentialReadResult(oauthcredentials.SourceFile, tokenWithExpiry(now.Add(time.Hour), true), now)
	selected, err := selectCredentialCandidate([]oauthcredentials.ReadResult{keychain, file})
	if err != nil {
		t.Fatalf("selectCredentialCandidate error = %v", err)
	}
	if selected.Source != oauthcredentials.SourceFile {
		t.Fatalf("source = %q, want credentials_file", selected.Source)
	}
}

func TestSelectCredentialCandidate_SummaryDoesNotLeakSecrets(t *testing.T) {
	now := oauthClock.Now()
	result := credentialReadResult(oauthcredentials.SourceFile, &Tokens{
		AccessToken:  "access-super-secret",
		RefreshToken: "refresh-super-secret",
		ExpiresAt:    now.Add(time.Hour).UnixMilli(),
	}, now)
	selected, err := selectCredentialCandidate([]oauthcredentials.ReadResult{result})
	if err != nil {
		t.Fatalf("selectCredentialCandidate error = %v", err)
	}
	encoded, err := json.Marshal(selected.Summaries)
	if err != nil {
		t.Fatalf("marshal summaries: %v", err)
	}
	encodedText := string(encoded)
	if strings.Contains(encodedText, "access-super-secret") || strings.Contains(encodedText, "refresh-super-secret") {
		t.Fatalf("summary leaked token value: %s", encodedText)
	}
}

func TestToken_RereadsWhenCachedTokenCannotRefresh(t *testing.T) {
	dir := t.TempDir()
	now := oauthClock.Now()
	writeOAuthCredentialFile(t, dir, tokenWithExpiry(now.Add(time.Hour), true))
	manager := NewManager(config.AdapterOAuth{}, dir)
	manager.cached = tokenWithExpiry(now.Add(-time.Hour), false)
	manager.snapshot = credentialSnapshot{
		Source:              oauthcredentials.SourceKeychain,
		RefreshTokenPresent: false,
	}
	token, err := manager.Token(context.Background())
	if err != nil {
		t.Fatalf("Token error = %v", err)
	}
	if token != "access-token" {
		t.Fatalf("token = %q, want access-token", token)
	}
	if manager.snapshot.Source != oauthcredentials.SourceFile {
		t.Fatalf("cached source = %q, want credentials_file", manager.snapshot.Source)
	}
}

func TestToken_DarwinKeychainChangeInvalidatesUnexpiredCache(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "keychain.json")
	writeFakeKeychainState(t, statePath, tokenWithAccess("keychain-access-one", nowPlus(time.Hour), true))
	t.Setenv("CLYDE_TEST_SECURITY_STATE", statePath)
	t.Setenv("CLYDE_TEST_SECURITY_ACCOUNT", "test-account")

	manager := NewManager(config.AdapterOAuth{
		KeychainService: "Claude Code-credentials",
	}, filepath.Join(dir, "claude"))
	manager.platform = "darwin"
	manager.securityBinary = fakeSecurityPath(t)

	token, err := manager.Token(context.Background())
	if err != nil {
		t.Fatalf("Token initial error = %v", err)
	}
	if token != "keychain-access-one" {
		t.Fatalf("initial token = %q, want keychain-access-one", token)
	}

	writeFakeKeychainState(t, statePath, tokenWithAccess("keychain-access-two", nowPlus(time.Hour), true))
	token, err = manager.Token(context.Background())
	if err != nil {
		t.Fatalf("Token after keychain change error = %v", err)
	}
	if token != "keychain-access-two" {
		t.Fatalf("token after keychain change = %q, want keychain-access-two", token)
	}
	if manager.snapshot.Source != oauthcredentials.SourceKeychain {
		t.Fatalf("cached source = %q, want keychain", manager.snapshot.Source)
	}
}

func TestToken_LinuxFileDeletionInvalidatesUnexpiredCache(t *testing.T) {
	dir := t.TempDir()
	writeOAuthCredentialFile(t, dir, tokenWithAccess("file-access-one", nowPlus(time.Hour), true))
	manager := NewManager(config.AdapterOAuth{}, dir)
	manager.platform = "linux"

	token, err := manager.Token(context.Background())
	if err != nil {
		t.Fatalf("Token initial error = %v", err)
	}
	if token != "file-access-one" {
		t.Fatalf("initial token = %q, want file-access-one", token)
	}

	if err := os.Remove(filepath.Join(dir, ".credentials.json")); err != nil {
		t.Fatalf("remove credentials file: %v", err)
	}
	_, err = manager.Token(context.Background())
	if err == nil {
		t.Fatal("Token after deletion error = nil, want error")
	}
	if manager.cached != nil {
		t.Fatal("cache remained populated after authoritative file deletion")
	}
}

func TestToken_LinuxFileRewriteInvalidatesUnexpiredCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	tokens := tokenWithAccess("file-access-one", nowPlus(time.Hour), true)
	writeOAuthCredentialFile(t, dir, tokens)
	setFileMtime(t, path, time.Unix(1000, 0))
	manager := NewManager(config.AdapterOAuth{}, dir)
	manager.platform = "linux"

	token, err := manager.Token(context.Background())
	if err != nil {
		t.Fatalf("Token initial error = %v", err)
	}
	if token != "file-access-one" {
		t.Fatalf("initial token = %q, want file-access-one", token)
	}
	initialMtime := manager.snapshot.FileMtime

	writeOAuthCredentialFile(t, dir, tokens)
	setFileMtime(t, path, time.Unix(2000, 0))
	token, err = manager.Token(context.Background())
	if err != nil {
		t.Fatalf("Token after rewrite error = %v", err)
	}
	if token != "file-access-one" {
		t.Fatalf("token after rewrite = %q, want file-access-one", token)
	}
	if manager.snapshot.FileMtime == initialMtime {
		t.Fatalf("cache file mtime = %d, want rewritten mtime", manager.snapshot.FileMtime)
	}
}

func TestToken_LinuxFileFingerprintChangeInvalidatesUnexpiredCache(t *testing.T) {
	dir := t.TempDir()
	writeOAuthCredentialFile(t, dir, tokenWithAccess("file-access-one", nowPlus(time.Hour), true))
	manager := NewManager(config.AdapterOAuth{}, dir)
	manager.platform = "linux"

	token, err := manager.Token(context.Background())
	if err != nil {
		t.Fatalf("Token initial error = %v", err)
	}
	if token != "file-access-one" {
		t.Fatalf("initial token = %q, want file-access-one", token)
	}
	initialFingerprint := manager.snapshot.Fingerprint

	writeOAuthCredentialFile(t, dir, tokenWithAccess("file-access-two", nowPlus(time.Hour), true))
	token, err = manager.Token(context.Background())
	if err != nil {
		t.Fatalf("Token after fingerprint change error = %v", err)
	}
	if token != "file-access-two" {
		t.Fatalf("token after fingerprint change = %q, want file-access-two", token)
	}
	if manager.snapshot.Fingerprint == initialFingerprint {
		t.Fatal("cache fingerprint did not change after authoritative file rewrite")
	}
}

func TestRefreshLocked_UsesTypedPayloadAndWritesFile(t *testing.T) {
	dir := t.TempDir()
	var requestBody oauthRefreshRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"refreshed-access","refresh_token":"refreshed-refresh","expires_in":3600,"scope":"user:profile user:inference"}`))
	}))
	defer server.Close()
	manager := NewManager(config.AdapterOAuth{
		TokenURL: server.URL,
		ClientID: "client-id",
		Scopes:   []string{"user:profile", "user:inference"},
	}, dir)
	selected := &selectedCredential{
		Source: oauthcredentials.SourceFile,
		Tokens: &Tokens{
			AccessToken:  "expired-access",
			RefreshToken: "refresh-input",
			ExpiresAt:    oauthClock.Now().Add(-time.Hour).UnixMilli(),
		},
	}
	refreshed, err := manager.refreshLocked(context.Background(), selected)
	if err != nil {
		t.Fatalf("refreshLocked error = %v", err)
	}
	if refreshed.AccessToken != "refreshed-access" {
		t.Fatalf("refreshed access token = %q, want refreshed-access", refreshed.AccessToken)
	}
	if requestBody.GrantType != "refresh_token" || requestBody.RefreshToken != "refresh-input" || requestBody.ClientID != "client-id" {
		t.Fatalf("request body = %+v, want typed refresh payload", requestBody)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		t.Fatalf("read written credentials: %v", err)
	}
	if !strings.Contains(string(data), "refreshed-access") {
		t.Fatal("written credentials did not include refreshed access token")
	}
}

func TestRefreshLocked_WritesSelectedKeychainStore(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "keychain.json")
	argsPath := filepath.Join(dir, "args.txt")
	writeFakeKeychainState(t, statePath, tokenWithAccess("expired-access", nowPlus(-time.Hour), true))
	t.Setenv("CLYDE_TEST_SECURITY_STATE", statePath)
	t.Setenv("CLYDE_TEST_SECURITY_ARGS", argsPath)
	t.Setenv("CLYDE_TEST_SECURITY_ACCOUNT", "test-account")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"keychain-refreshed-access","refresh_token":"keychain-refreshed-refresh","expires_in":3600,"scope":"user:profile"}`))
	}))
	defer server.Close()

	manager := NewManager(config.AdapterOAuth{
		TokenURL:        server.URL,
		ClientID:        "client-id",
		Scopes:          []string{"user:profile"},
		KeychainService: "Claude Code-credentials",
	}, filepath.Join(dir, "claude"))
	manager.platform = "darwin"
	manager.securityBinary = fakeSecurityPath(t)
	selected := &selectedCredential{
		Source: oauthcredentials.SourceKeychain,
		Tokens: tokenWithAccess("expired-access", nowPlus(-time.Hour), true),
	}

	refreshed, err := manager.refreshLocked(context.Background(), selected)
	if err != nil {
		t.Fatalf("refreshLocked error = %v", err)
	}
	if refreshed.AccessToken != "keychain-refreshed-access" {
		t.Fatalf("refreshed access token = %q, want keychain-refreshed-access", refreshed.AccessToken)
	}
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read fake keychain state: %v", err)
	}
	if !strings.Contains(string(stateData), "keychain-refreshed-access") {
		t.Fatal("fake keychain state did not include refreshed access token")
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake security args: %v", err)
	}
	argsText := string(argsData)
	if strings.Contains(argsText, "keychain-refreshed-access") || strings.Contains(argsText, "keychain-refreshed-refresh") {
		t.Fatalf("keychain command args leaked token value: %s", argsText)
	}
}

func TestRefreshLocked_FailsWhenPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "keychain.json")
	writeFakeKeychainState(t, statePath, tokenWithAccess("expired-access", nowPlus(-time.Hour), true))
	t.Setenv("CLYDE_TEST_SECURITY_STATE", statePath)
	t.Setenv("CLYDE_TEST_SECURITY_FAIL_ADD", "1")
	t.Setenv("CLYDE_TEST_SECURITY_ACCOUNT", "test-account")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"keychain-refreshed-access","refresh_token":"keychain-refreshed-refresh","expires_in":3600,"scope":"user:profile"}`))
	}))
	defer server.Close()

	manager := NewManager(config.AdapterOAuth{
		TokenURL:        server.URL,
		ClientID:        "client-id",
		Scopes:          []string{"user:profile"},
		KeychainService: "Claude Code-credentials",
	}, filepath.Join(dir, "claude"))
	manager.platform = "darwin"
	manager.securityBinary = fakeSecurityPath(t)
	selected := &selectedCredential{
		Source: oauthcredentials.SourceKeychain,
		Tokens: tokenWithAccess("expired-access", nowPlus(-time.Hour), true),
	}

	_, err := manager.refreshLocked(context.Background(), selected)
	if err == nil {
		t.Fatal("refreshLocked error = nil, want persistence error")
	}
	errText := err.Error()
	if !strings.Contains(errText, "persist refreshed oauth credentials") {
		t.Fatalf("refreshLocked error = %q, want persistence context", errText)
	}
	if strings.Contains(errText, "keychain-refreshed-access") || strings.Contains(errText, "keychain-refreshed-refresh") {
		t.Fatalf("refreshLocked error leaked token value: %q", errText)
	}
}

func TestRefreshLocked_StatusErrorDoesNotLeakTokenValues(t *testing.T) {
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh-input keychain-refreshed-access"}`))
	}))
	defer server.Close()

	manager := NewManager(config.AdapterOAuth{
		TokenURL: server.URL,
		ClientID: "client-id",
		Scopes:   []string{"user:profile"},
	}, dir)
	selected := &selectedCredential{
		Source: oauthcredentials.SourceFile,
		Tokens: &Tokens{
			AccessToken:  "expired-access",
			RefreshToken: "refresh-input",
			ExpiresAt:    oauthClock.Now().Add(-time.Hour).UnixMilli(),
		},
	}

	_, err := manager.refreshLocked(context.Background(), selected)
	if err == nil {
		t.Fatal("refreshLocked error = nil, want refresh status error")
	}
	errText := err.Error()
	if !strings.Contains(errText, "invalid_grant") {
		t.Fatalf("refreshLocked error = %q, want invalid_grant classification", errText)
	}
	if strings.Contains(errText, "refresh-input") || strings.Contains(errText, "keychain-refreshed-access") {
		t.Fatalf("refreshLocked error leaked token value: %q", errText)
	}
}

func credentialReadResult(source oauthcredentials.Source, tokens *Tokens, now time.Time) oauthcredentials.ReadResult {
	metadata := oauthcredentials.NewMetadata(tokens, now, 0)
	return oauthcredentials.ReadResult{
		Source:   source,
		Tokens:   tokens,
		Present:  tokens != nil,
		Metadata: metadata,
	}
}

func tokenWithExpiry(expiresAt time.Time, refreshTokenPresent bool) *Tokens {
	return tokenWithAccess("access-token", expiresAt, refreshTokenPresent)
}

func tokenWithAccess(accessToken string, expiresAt time.Time, refreshTokenPresent bool) *Tokens {
	refreshToken := ""
	if refreshTokenPresent {
		refreshToken = "refresh-token"
	}
	return &Tokens{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt.UnixMilli(),
	}
}

func nowPlus(duration time.Duration) time.Time {
	return oauthClock.Now().Add(duration)
}

func writeOAuthCredentialFile(t *testing.T, dir string, tokens *Tokens) {
	t.Helper()
	encoded, err := json.Marshal(struct {
		ClaudeAIOauth *Tokens `json:"claudeAiOauth"`
	}{ClaudeAIOauth: tokens})
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), encoded, 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
}

func writeFakeKeychainState(t *testing.T, path string, tokens *Tokens) {
	t.Helper()
	encoded, err := json.Marshal(struct {
		ClaudeAIOauth *Tokens `json:"claudeAiOauth"`
	}{ClaudeAIOauth: tokens})
	if err != nil {
		t.Fatalf("marshal fake keychain state: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write fake keychain state: %v", err)
	}
}

func setFileMtime(t *testing.T, path string, timestamp time.Time) {
	t.Helper()
	if err := os.Chtimes(path, timestamp, timestamp); err != nil {
		t.Fatalf("set file mtime: %v", err)
	}
}

func fakeSecurityPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "providers", "claude", "oauthcredentials", "testdata", "fake_security.sh")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod fake security: %v", err)
	}
	return path
}
