package oauthcredentials

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
