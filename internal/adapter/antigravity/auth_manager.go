package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
)

const (
	// #nosec G101 -- the Google OAuth token endpoint is public.
	oauthTokenEndpoint       = "https://oauth2.googleapis.com/token"
	invalidGrantCode         = "invalid_grant"
	refreshSafetyWindow      = 60 * time.Second
	refreshHTTPTimeout       = 30 * time.Second
	refreshResponseBodyLimit = 1 << 20
)

var (
	errRefreshTokenMissing = errors.New("refresh token is missing")
	errTokenEndpointStatus = errors.New("token endpoint returned an error status")
	errTokenResponseEmpty  = errors.New("token endpoint returned an incomplete response")
)

// AuthRefreshError reports a failure while refreshing Antigravity credentials.
type AuthRefreshError struct {
	Permanent bool
	Code      string
	Cause     error
}

// Error returns the safe message for the refresh failure.
//
// The message includes sign in instructions for invalid grants.
//
//	message := refreshErr.Error()
func (e *AuthRefreshError) Error() string {
	if e == nil {
		return ""
	}

	if e.Code == invalidGrantCode {
		return "Antigravity refresh token is invalid; sign in to Antigravity again"
	}

	if e.Cause != nil {
		return fmt.Sprintf("Antigravity token refresh failed: %v", e.Cause)
	}

	return "Antigravity token refresh failed"
}

// Unwrap returns the underlying refresh error.
//
// Callers can inspect transport failures with [errors.Is].
//
//	cause := refreshErr.Unwrap()
func (e *AuthRefreshError) Unwrap() error { return e.Cause }

// AuthManager refreshes access tokens in memory without changing the login
// source. Token rereads the source before using a cached token, then refreshes
// credentials that expire within 60 seconds. Callers can use ForceRefresh after
// an upstream authentication failure.
type AuthManager struct {
	mu           sync.Mutex
	source       CredentialSource
	client       *http.Client
	clientID     string
	clientSecret string
	endpoint     string
	now          func() time.Time
	cached       Credential
	snapshot     string
}

var _ adapterprovider.AuthRefresher = (*AuthManager)(nil)

// AuthManagerOptions injects the HTTP client, OAuth client values, endpoint,
// and clock. Tests can point Endpoint at an in-memory HTTP server.
type AuthManagerOptions struct {
	HTTPClient   *http.Client
	ClientID     string
	ClientSecret string
	Endpoint     string
	Now          func() time.Time
}

// NewAuthManager creates an in-memory Antigravity token manager.
//
// It rereads credentials before each Token call.
//
//	manager := NewAuthManager(source, options)
func NewAuthManager(source CredentialSource, options AuthManagerOptions) *AuthManager {
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: refreshHTTPTimeout}
	}

	endpoint := strings.TrimSpace(options.Endpoint)
	if endpoint == "" {
		endpoint = oauthTokenEndpoint
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}

	return &AuthManager{
		mu:           sync.Mutex{},
		source:       source,
		client:       client,
		clientID:     options.ClientID,
		clientSecret: options.ClientSecret,
		endpoint:     endpoint,
		now:          now,
		cached: Credential{
			AccessToken:  "",
			RefreshToken: "",
			ExpiresAt:    time.Time{},
			Fingerprint:  "",
		},
		snapshot: "",
	}
}

// Token returns an access token.
//
// It refreshes the token when it expires within 60 seconds.
//
//	token, err := manager.Token(ctx)
func (m *AuthManager) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	credential, err := m.readCredential(ctx)
	if err != nil {
		return "", err
	}

	if m.snapshot != credential.Fingerprint {
		m.cached = credential
		m.snapshot = credential.Fingerprint
	}

	if m.cached.AccessToken != "" && m.cached.ExpiresAt.After(m.now().Add(refreshSafetyWindow)) {
		return m.cached.AccessToken, nil
	}

	return m.refresh(ctx, credential)
}

// ForceRefresh refreshes the current credential after an upstream auth failure.
//
// It rereads the source before sending the OAuth request.
//
//	token, err := manager.ForceRefresh(ctx)
func (m *AuthManager) ForceRefresh(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	credential, err := m.readCredential(ctx)
	if err != nil {
		return "", err
	}

	if m.snapshot != credential.Fingerprint {
		m.cached = credential
		m.snapshot = credential.Fingerprint
	}

	return m.refresh(ctx, credential)
}

func (m *AuthManager) readCredential(ctx context.Context) (Credential, error) {
	credential, err := m.source.Read(ctx)
	if err != nil {
		wrapped := fmt.Errorf("read Antigravity credential source %s: %w", m.source.Source(), err)
		slog.ErrorContext(
			ctx,
			"adapter.antigravity.auth.source_read_failed",
			"concern",
			"adapter.providers.antigravity.oauth",
			"component",
			"adapter",
			"subcomponent",
			"antigravity_auth",
			"err",
			wrapped,
		)

		return Credential{
			AccessToken:  "",
			RefreshToken: "",
			ExpiresAt:    time.Time{},
			Fingerprint:  "",
		}, wrapped
	}

	return credential, nil
}

func (m *AuthManager) refresh(ctx context.Context, credential Credential) (string, error) {
	if credential.RefreshToken == "" {
		return "", &AuthRefreshError{
			Permanent: true,
			Code:      invalidGrantCode,
			Cause:     errRefreshTokenMissing,
		}
	}

	form := url.Values{}
	form.Set("client_id", m.clientID)
	form.Set("client_secret", m.clientSecret)
	form.Set("refresh_token", credential.RefreshToken)
	form.Set("grant_type", "refresh_token")
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		m.endpoint,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", &AuthRefreshError{
			Permanent: false,
			Code:      "",
			Cause:     fmt.Errorf("create token refresh request: %w", err),
		}
	}

	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := m.client.Do(request)
	if err != nil {
		return "", &AuthRefreshError{
			Permanent: false,
			Code:      "",
			Cause:     fmt.Errorf("send token refresh request: %w", err),
		}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, refreshResponseBodyLimit))
	if err != nil {
		return "", &AuthRefreshError{
			Permanent: false,
			Code:      "",
			Cause:     fmt.Errorf("read token refresh response: %w", err),
		}
	}

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", &AuthRefreshError{
			Permanent: false,
			Code:      "",
			Cause:     fmt.Errorf("decode token refresh response: %w", err),
		}
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		permanent := result.Error == invalidGrantCode

		return "", &AuthRefreshError{
			Permanent: permanent,
			Code:      result.Error,
			Cause:     fmt.Errorf("%w: HTTP %d", errTokenEndpointStatus, response.StatusCode),
		}
	}

	if result.AccessToken == "" || result.ExpiresIn <= 0 {
		return "", &AuthRefreshError{
			Permanent: false,
			Code:      "",
			Cause:     errTokenResponseEmpty,
		}
	}

	m.cached.AccessToken = result.AccessToken
	m.cached.ExpiresAt = m.now().Add(time.Duration(result.ExpiresIn) * time.Second)

	return m.cached.AccessToken, nil
}
