//go:build darwin

package oauthcredentials

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/keybase/go-keychain"
)

// TestWriteKeychainRoundTrip writes a credential under a test-only service
// name and reads it back via the same reader the production code uses. The
// assertion is that the JSON payload round-trips byte-for-byte through the
// keychain so the rotator's mirror sync and the in-use detector observe
// what the writer just stored.
func TestWriteKeychainRoundTrip(t *testing.T) {
	const testService = "io.goodkind.clyde-test-credentials"
	t.Cleanup(func() {
		_ = keychain.DeleteGenericPasswordItem(testService, currentUserForTest(t))
	})

	writer := &keychainWriter{
		service:         testService,
		mu:              sync.Mutex{},
		macOSAccountSet: false,
		macOSAccount:    "",
	}

	doc := Document{ClaudeAIOauth: &Tokens{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Scopes:       []string{"user:inference"},
	}}
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := writer.write(context.Background(), payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	reader := keychainStore{
		keychainService: testService,
		now:             time.Now(),
	}
	result := reader.Read(context.Background())
	if result.Err != nil {
		t.Fatalf("read: %v", result.Err)
	}
	if !result.Present || result.Tokens == nil {
		t.Fatalf("read: expected tokens present, got result=%+v", result)
	}
	if result.Tokens.AccessToken != "test-access-token" {
		t.Fatalf("AccessToken = %q, want test-access-token", result.Tokens.AccessToken)
	}
	if result.Tokens.RefreshToken != "test-refresh-token" {
		t.Fatalf("RefreshToken = %q, want test-refresh-token", result.Tokens.RefreshToken)
	}
}

// TestWriteKeychainRoundTripUpdatesExisting verifies the second write to the
// same service updates the existing entry rather than failing on a
// duplicate. The new bytes must be observable on the next read.
func TestWriteKeychainRoundTripUpdatesExisting(t *testing.T) {
	const testService = "io.goodkind.clyde-test-credentials-update"
	t.Cleanup(func() {
		_ = keychain.DeleteGenericPasswordItem(testService, currentUserForTest(t))
	})

	writer := &keychainWriter{
		service:         testService,
		mu:              sync.Mutex{},
		macOSAccountSet: false,
		macOSAccount:    "",
	}

	doc1 := Document{ClaudeAIOauth: &Tokens{
		AccessToken:  "first-access",
		RefreshToken: "first-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}}
	first, err := json.Marshal(doc1)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	if err := writer.write(context.Background(), first); err != nil {
		t.Fatalf("write first: %v", err)
	}

	doc2 := Document{ClaudeAIOauth: &Tokens{
		AccessToken:  "second-access",
		RefreshToken: "second-refresh",
		ExpiresAt:    time.Now().Add(2 * time.Hour).UnixMilli(),
	}}
	second, err := json.Marshal(doc2)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if err := writer.write(context.Background(), second); err != nil {
		t.Fatalf("write second: %v", err)
	}

	reader := keychainStore{
		keychainService: testService,
		now:             time.Now(),
	}
	result := reader.Read(context.Background())
	if result.Err != nil {
		t.Fatalf("read: %v", result.Err)
	}
	if result.Tokens == nil || result.Tokens.AccessToken != "second-access" {
		t.Fatalf("update did not persist: got result=%+v", result)
	}
}

// TestWriteKeychainNoSecurityBinary verifies the writer does not depend on
// the legacy /usr/bin/security subprocess. Setting PATH empty makes the
// `security` binary unfindable; the write must still succeed because the
// new implementation calls the Security framework directly via cgo. If a
// regression reintroduces an exec.Command shell-out, exec.LookPath fails
// inside that code path and this test catches it.
func TestWriteKeychainNoSecurityBinary(t *testing.T) {
	const testService = "io.goodkind.clyde-test-no-binary"
	t.Cleanup(func() {
		_ = keychain.DeleteGenericPasswordItem(testService, currentUserForTest(t))
	})

	t.Setenv("PATH", "")

	writer := &keychainWriter{
		service:         testService,
		mu:              sync.Mutex{},
		macOSAccountSet: false,
		macOSAccount:    "",
	}

	doc := Document{ClaudeAIOauth: &Tokens{
		AccessToken:  "no-binary-access",
		RefreshToken: "no-binary-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}}
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := writer.write(context.Background(), payload); err != nil {
		t.Fatalf("write with empty PATH: %v", err)
	}

	reader := keychainStore{
		keychainService: testService,
		now:             time.Now(),
	}
	result := reader.Read(context.Background())
	if result.Err != nil {
		t.Fatalf("read with empty PATH: %v", result.Err)
	}
	if !result.Present || result.Tokens == nil || result.Tokens.AccessToken != "no-binary-access" {
		t.Fatalf("round-trip with empty PATH: got result=%+v", result)
	}
}

// currentUserForTest reads the login user the same way the production
// writer resolves the account name on a fresh keychain. The test uses it to
// scope the cleanup delete to the entry the writer just created.
func currentUserForTest(t *testing.T) string {
	t.Helper()
	username, err := readLoginUser()
	if err != nil {
		t.Fatalf("loginUser: %v", err)
	}
	return username
}
