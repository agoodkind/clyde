package antigravity_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/adapter/antigravity"
	"goodkind.io/clyde/internal/providers/antigravity/oauthcredentials"
)

func TestAuthManagerRefreshDoesNotChangeTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "antigravity-oauth-token")
	initial := []byte(
		`{"token":{"access_token":"old","refresh_token":"refresh","expiry":"2020-01-01T00:00:00Z"}}`,
	)
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fixedTime := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, fixedTime, fixedTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat before refresh: %v", err)
	}

	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile before refresh: %v", err)
	}

	server := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"access_token":"new","expires_in":3600}`))
		}),
	)
	defer server.Close()
	source, err := oauthcredentials.NewSourceWithPlatform(path, "linux")
	if err != nil {
		t.Fatalf("NewSourceWithPlatform: %v", err)
	}

	manager := antigravity.NewAuthManager(
		source,
		antigravity.AuthManagerOptions{
			HTTPClient: server.Client(),
			Endpoint:   server.URL,
		},
	)
	if _, err := manager.ForceRefresh(context.Background()); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}

	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat after refresh: %v", err)
	}

	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after refresh: %v", err)
	}

	if string(afterBytes) != string(beforeBytes) ||
		!afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatal("token file content or modification time changed during refresh")
	}
}
