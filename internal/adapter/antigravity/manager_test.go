package antigravity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type testCredentialSource struct {
	credential Credential
	readCount  int
}

func (source *testCredentialSource) Read(context.Context) (Credential, error) {
	source.readCount++

	return source.credential, nil
}

func (*testCredentialSource) Source() string { return "fixture" }

func TestAuthManagerRefreshRequest(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", request.Method)
			}

			if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type = %q", request.Header.Get("Content-Type"))
			}

			if err := request.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}

			want := url.Values{
				"client_id":     {"client-id"},
				"client_secret": {"client-secret"},
				"refresh_token": {"refresh-token"},
				"grant_type":    {"refresh_token"},
			}
			if request.PostForm.Encode() != want.Encode() {
				t.Errorf("form = %v, want %v", request.PostForm, want)
			}

			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"access_token":"new-token","expires_in":3600}`))
		}),
	)
	defer server.Close()

	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	source := &testCredentialSource{
		credential: Credential{
			AccessToken:  "old-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    now,
			Fingerprint:  "one",
		},
	}
	manager := NewAuthManager(
		source,
		AuthManagerOptions{
			HTTPClient:   server.Client(),
			ClientID:     "client-id",
			ClientSecret: "client-secret",
			Endpoint:     server.URL,
			Now:          func() time.Time { return now },
		},
	)
	token, err := manager.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	if token != "new-token" {
		t.Fatalf("Token = %q, want new-token", token)
	}
}

func TestAuthManagerInvalidGrantIsPermanent(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"error":"invalid_grant"}`))
		}),
	)
	defer server.Close()

	manager := NewAuthManager(
		&testCredentialSource{
			credential: Credential{
				RefreshToken: "refresh-token",
				Fingerprint:  "one",
			},
		},
		AuthManagerOptions{HTTPClient: server.Client(), Endpoint: server.URL},
	)
	_, err := manager.ForceRefresh(context.Background())
	var refreshError *AuthRefreshError
	if !errors.As(err, &refreshError) || !refreshError.Permanent {
		t.Fatalf("ForceRefresh error = %v, want permanent AuthRefreshError", err)
	}

	if got := refreshError.Error(); got != "Antigravity refresh token is invalid; sign in to Antigravity again" {
		t.Fatalf("error message = %q", got)
	}
}

func TestAuthManagerInvalidClientIsPermanent(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write(
				[]byte(`{"error":"invalid_client","error_description":"fixture-private-detail"}`),
			)
		}),
	)
	defer server.Close()

	source := &testCredentialSource{
		credential: Credential{
			RefreshToken: "fixture-refresh-token",
			Fingerprint:  "fixture",
		},
	}
	manager := NewAuthManager(
		source,
		AuthManagerOptions{
			HTTPClient: server.Client(),
			Endpoint:   server.URL,
		},
	)
	_, err := manager.ForceRefresh(t.Context())
	var refreshError *AuthRefreshError
	if !errors.As(err, &refreshError) || !refreshError.Permanent ||
		refreshError.Code != invalidClientCode {
		t.Fatalf("ForceRefresh error = %v, want permanent invalid_client error", err)
	}

	const message = "Antigravity OAuth client was rejected. Check adapter.antigravity.client_id and the client secret setting"
	if refreshError.Error() != message {
		t.Fatalf("error message = %q", refreshError.Error())
	}
}

func TestAuthManagerReadsSourceAfterFingerprintChange(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			requestCount++
			_, _ = writer.Write([]byte(`{"access_token":"fresh-token","expires_in":3600}`))
		}),
	)
	defer server.Close()

	now := time.Now()
	source := &testCredentialSource{
		credential: Credential{
			AccessToken: "initial-token",
			ExpiresAt:   now.Add(time.Hour),
			Fingerprint: "one",
		},
	}
	manager := NewAuthManager(
		source,
		AuthManagerOptions{
			HTTPClient: server.Client(),
			Endpoint:   server.URL,
			Now:        func() time.Time { return now },
		},
	)
	if _, err := manager.Token(context.Background()); err != nil {
		t.Fatalf("initial Token: %v", err)
	}

	source.credential = Credential{
		RefreshToken: "replacement-refresh-token",
		ExpiresAt:    now,
		Fingerprint:  "two",
	}
	if token, err := manager.Token(context.Background()); err != nil || token != "fresh-token" {
		t.Fatalf("Token after fingerprint change = %q, %v", token, err)
	}

	if source.readCount != 2 || requestCount != 1 {
		t.Fatalf(
			"source reads = %d, refresh calls = %d, want 2 and 1",
			source.readCount,
			requestCount,
		)
	}
}
