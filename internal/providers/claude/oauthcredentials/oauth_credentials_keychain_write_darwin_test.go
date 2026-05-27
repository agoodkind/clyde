//go:build darwin

package oauthcredentials

import (
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// TestWriteKeychainRoundTrip writes a credential under a test-only service
// name and reads it back via the same reader the production code uses. The
// assertion is that the JSON payload round-trips byte-for-byte through the
// keychain so the rotator's mirror sync and the in-use detector observe
// what the writer just stored.
func TestWriteKeychainRoundTrip(t *testing.T) {
	const testService = "io.goodkind.clyde-test-credentials"
	t.Cleanup(func() {
		_ = exec.Command(securityBinaryPath, "delete-generic-password", "-s", testService).Run() //nolint:gosec // test cleanup; service is a typed constant.
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
		_ = exec.Command(securityBinaryPath, "delete-generic-password", "-s", testService).Run() //nolint:gosec // test cleanup; service is a typed constant.
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

// TestKeychainReadMissingEntry verifies the reader maps the security tool's
// "item not found" exit code (44) to a Present=false result with no error,
// so callers distinguish "no credential configured" from a genuine read
// failure.
func TestKeychainReadMissingEntry(t *testing.T) {
	const testService = "io.goodkind.clyde-test-credentials-missing"
	// Pre-cleanup in case a prior aborted run left the entry behind.
	_ = exec.Command(securityBinaryPath, "delete-generic-password", "-s", testService).Run() //nolint:gosec // test setup; service is a typed constant.

	reader := keychainStore{
		keychainService: testService,
		now:             time.Now(),
	}
	result := reader.Read(context.Background())
	if result.Err != nil {
		t.Fatalf("read on missing entry returned error: %v", result.Err)
	}
	if result.Present {
		t.Fatalf("read on missing entry returned Present=true: result=%+v", result)
	}
	if result.Tokens != nil {
		t.Fatalf("read on missing entry returned non-nil tokens: result=%+v", result)
	}
}

// TestKeychainReadShellOutSurvivesEmptyPath verifies the reader uses an
// absolute path to /usr/bin/security and therefore works with PATH unset.
// PATH is irrelevant when exec.Command receives a fully qualified path,
// which is the invariant the production code depends on so the daemon's
// harvest loop is not affected by operator shell environment variations.
func TestKeychainReadShellOutSurvivesEmptyPath(t *testing.T) {
	const testService = "io.goodkind.clyde-test-credentials-path"
	t.Cleanup(func() {
		_ = exec.Command(securityBinaryPath, "delete-generic-password", "-s", testService).Run() //nolint:gosec // test cleanup; service is a typed constant.
	})

	writer := &keychainWriter{
		service:         testService,
		mu:              sync.Mutex{},
		macOSAccountSet: false,
		macOSAccount:    "",
	}

	doc := Document{ClaudeAIOauth: &Tokens{
		AccessToken:  "empty-path-access",
		RefreshToken: "empty-path-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}}
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := writer.write(context.Background(), payload); err != nil {
		t.Fatalf("write before clearing PATH: %v", err)
	}

	t.Setenv("PATH", "")

	reader := keychainStore{
		keychainService: testService,
		now:             time.Now(),
	}
	result := reader.Read(context.Background())
	if result.Err != nil {
		t.Fatalf("read with empty PATH: %v", result.Err)
	}
	if !result.Present || result.Tokens == nil || result.Tokens.AccessToken != "empty-path-access" {
		t.Fatalf("round-trip with empty PATH: got result=%+v", result)
	}
}
