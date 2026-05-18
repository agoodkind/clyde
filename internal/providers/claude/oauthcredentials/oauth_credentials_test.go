package oauthcredentials

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadCandidates_ReadsFileCredential(t *testing.T) {
	dir := t.TempDir()
	writeTestCredentials(t, dir, &Tokens{
		AccessToken:  "access-file-secret",
		RefreshToken: "refresh-file-secret",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Scopes:       []string{"user:profile", "user:inference"},
	})

	results := ReadCandidates(context.Background(), ReadOptions{
		CredentialsDir: dir,
		Now:            time.Now(),
	})
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	result := results[0]
	if result.Source != SourceFile {
		t.Fatalf("source = %q, want %q", result.Source, SourceFile)
	}
	if result.Err != nil {
		t.Fatalf("read err = %v, want nil", result.Err)
	}
	if !result.Present || result.Tokens == nil {
		t.Fatalf("present = %t tokens nil = %t, want present tokens", result.Present, result.Tokens == nil)
	}
	if !result.Metadata.AccessTokenPresent || !result.Metadata.RefreshTokenPresent {
		t.Fatalf("metadata token presence = %+v, want both true", result.Metadata)
	}
	if result.Metadata.Fingerprint == "" {
		t.Fatal("fingerprint is empty")
	}
	summaries := Summarize(results)
	encoded, err := json.Marshal(summaries)
	if err != nil {
		t.Fatalf("marshal summaries: %v", err)
	}
	encodedText := string(encoded)
	if strings.Contains(encodedText, "access-file-secret") || strings.Contains(encodedText, "refresh-file-secret") {
		t.Fatalf("summary leaked token value: %s", encodedText)
	}
}

func TestReadCandidates_DarwinKeychainPolicyDoesNotFallBackToFile(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "missing-keychain.json")
	writeTestCredentials(t, dir, &Tokens{
		AccessToken:  "access-file-secret",
		RefreshToken: "refresh-file-secret",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	})

	t.Setenv("CLYDE_TEST_SECURITY_STATE", statePath)
	results := ReadCandidates(context.Background(), ReadOptions{
		CredentialsDir:  dir,
		KeychainService: "Claude Code-credentials",
		SecurityBinary:  fakeSecurityPath(t),
		Platform:        "darwin",
		Now:             time.Now(),
	})

	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	result := results[0]
	if result.Source != SourceKeychain {
		t.Fatalf("source = %q, want %q", result.Source, SourceKeychain)
	}
	if result.Err != nil {
		t.Fatalf("read err = %v, want nil for missing keychain item", result.Err)
	}
	if result.Present || result.Tokens != nil {
		t.Fatal("missing keychain item fell back to a file credential")
	}
}

func TestReadCandidates_LinuxPolicyUsesFileEvenWithKeychainService(t *testing.T) {
	dir := t.TempDir()
	writeTestCredentials(t, dir, &Tokens{
		AccessToken:  "access-file-secret",
		RefreshToken: "refresh-file-secret",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	})

	results := ReadCandidates(context.Background(), ReadOptions{
		CredentialsDir:  dir,
		KeychainService: "Claude Code-credentials",
		SecurityBinary:  "/path/to/missing/security",
		Platform:        "linux",
		Now:             time.Now(),
	})

	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	result := results[0]
	if result.Source != SourceFile {
		t.Fatalf("source = %q, want %q", result.Source, SourceFile)
	}
	if result.Err != nil {
		t.Fatalf("read err = %v, want nil", result.Err)
	}
	if result.Tokens == nil || result.Tokens.AccessToken != "access-file-secret" {
		t.Fatal("tokens did not match the file credential")
	}
}

func TestWrite_FilePreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	initial := []byte(`{"mcpOAuth":{"server":{"accessToken":"other-secret"}}}`)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatalf("write initial credentials: %v", err)
	}
	tokens := &Tokens{
		AccessToken:  "new-access-secret",
		RefreshToken: "new-refresh-secret",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}
	result := Write(context.Background(), ReadOptions{
		CredentialsDir:  dir,
		KeychainService: "",
		SecurityBinary:  "",
		Platform:        "linux",
		Now:             time.Now(),
	}, SourceFile, tokens)
	if result.Err != nil {
		t.Fatalf("Write file error = %v", result.Err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("unmarshal credentials: %v", err)
	}
	if _, ok := document["mcpOAuth"]; !ok {
		t.Fatal("mcpOAuth key was not preserved")
	}
	if _, ok := document["claudeAiOauth"]; !ok {
		t.Fatal("claudeAiOauth key was not written")
	}
}

func TestWrite_KeychainWritesPasswordOnStdinWithoutTokenArguments(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "keychain.json")
	argsPath := filepath.Join(dir, "args.txt")
	initial := []byte(`{"mcpOAuth":{"server":{"accessToken":"other-secret"}}}`)
	if err := os.WriteFile(statePath, initial, 0o600); err != nil {
		t.Fatalf("write fake keychain state: %v", err)
	}
	t.Setenv("CLYDE_TEST_SECURITY_STATE", statePath)
	t.Setenv("CLYDE_TEST_SECURITY_ARGS", argsPath)
	t.Setenv("CLYDE_TEST_SECURITY_ACCOUNT", "test-account")

	tokens := &Tokens{
		AccessToken:  "new-access-secret",
		RefreshToken: "new-refresh-secret",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}
	result := Write(context.Background(), ReadOptions{
		KeychainService: "Claude Code-credentials",
		SecurityBinary:  fakeSecurityPath(t),
		Platform:        "darwin",
		Now:             time.Now(),
	}, SourceKeychain, tokens)
	if result.Err != nil {
		t.Fatalf("Write keychain error = %v", result.Err)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read fake keychain state: %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("unmarshal fake keychain state: %v", err)
	}
	if _, ok := document["mcpOAuth"]; !ok {
		t.Fatal("mcpOAuth key was not preserved")
	}
	if !strings.Contains(string(document["claudeAiOauth"]), "new-access-secret") {
		t.Fatal("claudeAiOauth was not written")
	}

	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake security args: %v", err)
	}
	argsText := string(argsData)
	if strings.Contains(argsText, "new-access-secret") || strings.Contains(argsText, "new-refresh-secret") {
		t.Fatal("keychain command args leaked token value")
	}
	if !strings.Contains(argsText, "-w") {
		t.Fatalf("keychain command args = %q, want prompt-form -w", argsText)
	}
}

func writeTestCredentials(t *testing.T, dir string, tokens *Tokens) {
	t.Helper()
	encoded, err := json.Marshal(struct {
		ClaudeAIOauth *Tokens `json:"claudeAiOauth"`
	}{ClaudeAIOauth: tokens})
	if err != nil {
		t.Fatalf("marshal test credentials: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), encoded, 0o600); err != nil {
		t.Fatalf("write test credentials: %v", err)
	}
}

func fakeSecurityPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("testdata", "fake_security.sh")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod fake security: %v", err)
	}
	return path
}
