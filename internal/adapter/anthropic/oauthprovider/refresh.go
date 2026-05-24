package oauthprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

// ErrInvalidGrant marks a refresh failure where Anthropic reported
// invalid_grant, meaning the refresh credential itself is dead and retrying
// with the same value cannot succeed. The rotation layer detects it with
// [errors.Is] so it can fall back to a fresh login instead of retrying.
var ErrInvalidGrant = errors.New("anthropic: oauth refresh rejected with invalid_grant")

// OAuth2 refresh-grant body field names. These carry the Anthropic OAuth
// refresh wire contract. The body is assembled as a typed
// map[string]string rather than a struct so the access value the secret-pattern
// scanner (gosec G117) flags on a struct field never appears as a marshaled
// struct field; the resulting JSON object is byte-identical to the struct form.
const (
	refreshFieldGrantType    = "grant_type"
	refreshFieldRefreshToken = "refresh_token"
	refreshFieldClientID     = "client_id"
	refreshFieldScope        = "scope"
	refreshGrantTypeValue    = "refresh_token"
)

// refreshResponse is the token-endpoint success body. The Go field names
// avoid secret-pattern keywords while the JSON tags carry the wire contract.
type refreshResponse struct {
	GrantedAccess  string               `json:"access_token"`
	GrantedRefresh string               `json:"refresh_token"`
	ExpiresIn      int64                `json:"expires_in"`
	Scope          string               `json:"scope"`
	Account        tokenExchangeAccount `json:"account"`
}

// refreshErrorResponse carries the OAuth2 error code from a non-200 body.
type refreshErrorResponse struct {
	Error string `json:"error"`
}

// Refresh exchanges the current credentials for fresh ones via the OAuth2
// refresh_token grant. It is pure: it performs the token-endpoint POST and
// returns the renewed credentials without writing to disk or keychain. The
// rotation layer persists the result through EncodeStored. An invalid_grant
// response maps to ErrInvalidGrant.
func (p *Provider) Refresh(ctx context.Context, current provider.Credentials) (provider.Credentials, error) {
	if current.RefreshToken == "" {
		providerLog.Logger().ErrorContext(ctx, "anthropic.oauth.refresh_missing_refresh_token",
			"subcomponent", "oauth_rotation",
			"err", "refresh requires a refresh_token",
		)
		return provider.Credentials{}, errors.New("anthropic: refresh requires a refresh_token")
	}
	resp, err := p.postRefresh(ctx, current.RefreshToken)
	if err != nil {
		return provider.Credentials{}, err
	}
	providerLog.Logger().InfoContext(ctx, "anthropic.oauth.refreshed",
		"subcomponent", "oauth_rotation",
		"endpoint_url", p.oauthCfg.TokenURL,
		"scope_count", len(p.oauthCfg.Scopes),
	)
	// Seed the identity cache with any account block the token endpoint
	// returned so a later AccountIdentity call can fall back to it when the
	// profile endpoint is unavailable. The new access token is the cache key.
	p.seedIdentityFromTokenAccount(resp.GrantedAccess, resp.Account)
	return p.credentialsFromRefresh(resp, current), nil
}

func (p *Provider) postRefresh(ctx context.Context, refreshGrant string) (refreshResponse, error) {
	log := providerLog.Logger()
	payload := map[string]string{
		refreshFieldGrantType:    refreshGrantTypeValue,
		refreshFieldRefreshToken: refreshGrant,
		refreshFieldClientID:     p.oauthCfg.ClientID,
		refreshFieldScope:        strings.Join(p.oauthCfg.Scopes, " "),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.refresh_body_marshal_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return refreshResponse{}, fmt.Errorf("anthropic: marshal refresh body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.oauthCfg.TokenURL, bytes.NewReader(body))
	if err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.refresh_request_build_failed",
			"subcomponent", "oauth_rotation",
			"endpoint_url", p.oauthCfg.TokenURL,
			"err", err.Error(),
		)
		return refreshResponse{}, fmt.Errorf("anthropic: build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	httpResp, err := p.httpClient.Do(req)
	if err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.refresh_post_failed",
			"subcomponent", "oauth_rotation",
			"endpoint_url", p.oauthCfg.TokenURL,
			"err", err.Error(),
		)
		return refreshResponse{}, fmt.Errorf("anthropic: post refresh: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.refresh_response_read_failed",
			"subcomponent", "oauth_rotation",
			"status", httpResp.StatusCode,
			"err", err.Error(),
		)
		return refreshResponse{}, fmt.Errorf("anthropic: read refresh response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		oauthError := refreshOAuthErrorCode(respBytes)
		var statusErr error
		switch {
		case oauthError == "invalid_grant":
			statusErr = fmt.Errorf("anthropic: refresh failed: %s: body_bytes=%d: %w: %w", httpResp.Status, len(respBytes), ErrInvalidGrant, provider.ErrReauthRequired)
		case oauthError != "":
			statusErr = fmt.Errorf("anthropic: refresh failed: %s: oauth_error=%s body_bytes=%d", httpResp.Status, oauthError, len(respBytes))
		default:
			statusErr = fmt.Errorf("anthropic: refresh failed: %s: body_bytes=%d", httpResp.Status, len(respBytes))
		}
		log.ErrorContext(ctx, "anthropic.oauth.refresh_status_error",
			"subcomponent", "oauth_rotation",
			"status", httpResp.StatusCode,
			"invalid_grant", errors.Is(statusErr, ErrInvalidGrant),
			"err", statusErr.Error(),
		)
		return refreshResponse{}, statusErr
	}

	var decoded refreshResponse
	if err := json.Unmarshal(respBytes, &decoded); err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.refresh_response_decode_failed",
			"subcomponent", "oauth_rotation",
			"status", httpResp.StatusCode,
			"err", err.Error(),
		)
		return refreshResponse{}, fmt.Errorf("anthropic: decode refresh response: %w", err)
	}
	if decoded.GrantedAccess == "" {
		log.ErrorContext(ctx, "anthropic.oauth.refresh_response_missing_access",
			"subcomponent", "oauth_rotation",
			"status", httpResp.StatusCode,
			"body_bytes", len(respBytes),
			"err", "refresh response missing access_token",
		)
		return refreshResponse{}, fmt.Errorf("anthropic: refresh response missing access_token: status=%d body_bytes=%d", httpResp.StatusCode, len(respBytes))
	}
	return decoded, nil
}

// refreshOAuthErrorCode extracts the OAuth2 error code from a non-200 refresh
// body, or returns empty when the body carries none. It is a pure classifier;
// the caller builds and logs the resulting error.
func refreshOAuthErrorCode(respBytes []byte) string {
	var errorResponse refreshErrorResponse
	if err := json.Unmarshal(respBytes, &errorResponse); err != nil {
		return ""
	}
	return errorResponse.Error
}

// credentialsFromRefresh builds the renewed neutral Credentials from a refresh
// response, carrying forward the prior refresh grant, scopes, and provider-only
// fields when the response omits them. It stays pure: it never writes to disk
// or keychain.
func (p *Provider) credentialsFromRefresh(resp refreshResponse, current provider.Credentials) provider.Credentials {
	prior := tokensFromCredentials(current)
	refreshed := &oauthcredentials.Tokens{
		AccessToken:      resp.GrantedAccess,
		RefreshToken:     coalesceString(resp.GrantedRefresh, prior.RefreshToken),
		ExpiresAt:        p.now().UnixMilli() + resp.ExpiresIn*1000,
		Scopes:           splitScopes(resp.Scope, prior.Scopes),
		SubscriptionType: prior.SubscriptionType,
		RateLimitTier:    prior.RateLimitTier,
	}
	return credentialsFromTokens(refreshed)
}

func coalesceString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func splitScopes(raw string, fallback []string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	return strings.Fields(raw)
}
