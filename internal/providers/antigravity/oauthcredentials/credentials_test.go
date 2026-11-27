package oauthcredentials

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const testTokenDocument = `{
	"token":{
		"access_token":"fixture-access",
		"token_type":"Bearer",
		"refresh_token":"fixture-refresh",
		"expiry":"2026-11-27T04:25:03Z"
	},
	"auth_method":"oauth",
	"id_token":"fixture-id"
}`

func TestCredentialFileReadsAntigravityDocument(t *testing.T) {
	path := writeTestToken(t, t.TempDir(), testTokenDocument)
	source := Source{stores: []Store{fileStore{path: path}}}

	credential, err := source.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	expiry := time.Date(2026, time.November, 27, 4, 25, 3, 0, time.UTC)
	if credential.AccessToken != "fixture-access" || credential.RefreshToken != "fixture-refresh" ||
		!credential.ExpiresAt.Equal(expiry) || credential.Fingerprint == "" {
		t.Fatal("credential did not match the stored token fields")
	}

	updated := strings.Replace(testTokenDocument, "fixture-access", "fixture-new-access", 1)
	writeTestToken(t, filepath.Dir(path), updated)
	reloaded, err := source.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if reloaded.Fingerprint == credential.Fingerprint {
		t.Fatal("fingerprint did not change after the token changed")
	}
}

func TestKeychainExit44FallsBackToCredentialFile(t *testing.T) {
	path := writeTestToken(t, t.TempDir(), testTokenDocument)
	keychain := testKeychain(t, testExitStatus(44), "")
	source := Source{stores: []Store{keychain, fileStore{path: path}}}

	credential, err := source.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if credential.AccessToken != "fixture-access" {
		t.Fatal("the token file did not supply the credential after a Keychain miss")
	}
}

func TestKeychainCredentialTakesPrecedenceOverFile(t *testing.T) {
	file := fileStore{path: writeTestToken(t, t.TempDir(), "malformed fixture")}
	keychain := testKeychain(t, nil, testTokenDocument)
	source := Source{stores: []Store{keychain, file}}

	credential, err := source.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if credential.AccessToken != "fixture-access" {
		t.Fatal("Keychain did not supply the credential")
	}
}

func writeTestToken(t *testing.T, dir, payload string) string {
	t.Helper()
	path := filepath.Join(dir, "antigravity-oauth-token")
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func testKeychain(t *testing.T, runErr error, payload string) keychainStore {
	t.Helper()

	return keychainStore{
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			t.Helper()
			want := []string{"find-generic-password", "-s", "gemini", "-a", "antigravity", "-w"}
			if name != "/usr/bin/security" || !slices.Equal(args, want) {
				t.Fatalf("Keychain command = %q %q", name, args)
			}

			return []byte(payload), runErr
		},
	}
}

type testExitStatus int

func (status testExitStatus) Error() string { return fmt.Sprintf("exit status %d", status) }
func (status testExitStatus) ExitCode() int { return int(status) }
