package oauthcredentials

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestWriteKeychainRoundTrip writes a credential under a test-only service
// name and reads it back via the existing reader, asserting the round-trip
// preserves the JSON bytes. The test is darwin-only because /usr/bin/security
// is a macOS facility; it skips when run on any other platform or when the
// security binary is unavailable.
func TestWriteKeychainRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("keychain round-trip requires macOS")
	}
	if _, err := exec.LookPath("security"); err != nil {
		t.Skipf("security binary not available: %v", err)
	}

	const testService = "io.goodkind.clyde-test-credentials"
	t.Cleanup(func() {
		_ = exec.Command("security", "delete-generic-password", "-s", testService).Run()
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
		securityBinary:  "security",
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

func TestWriteRejectsEmptyPayload(t *testing.T) {
	writer := &keychainWriter{
		service:         "io.goodkind.clyde-test-credentials-empty",
		mu:              sync.Mutex{},
		macOSAccountSet: false,
		macOSAccount:    "",
	}
	if err := writer.write(context.Background(), nil); err == nil {
		t.Fatal("write(nil): want error, got nil")
	}
}

func TestParseAcctAttribute(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "quoted_value",
			in: `keychain: "/Users/me/Library/Keychains/login.keychain-db"
class: "genp"
attributes:
    "acct"<blob>="alex"
    "svce"<blob>="Claude Code-credentials"
`,
			want: "alex",
		},
		{
			name: "no_acct_line",
			in:   `attributes:`,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAcctAttribute(tc.in); got != tc.want {
				t.Fatalf("parseAcctAttribute = %q, want %q", got, tc.want)
			}
		})
	}
}
