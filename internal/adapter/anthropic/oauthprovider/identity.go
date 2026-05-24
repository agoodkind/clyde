package oauthprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// profilePath is the Anthropic OAuth profile endpoint path appended to the API
// base host. The official Claude CLI builds it as `${BASE_API_URL}/api/oauth/
// profile` (src/services/oauth/getOauthProfile.ts), so Clyde matches that path
// exactly and derives the base host from the configured MessagesURL rather than
// hardcoding api.anthropic.com.
const profilePath = "/api/oauth/profile"

// profileTimeout caps a single profile round trip. The official Claude CLI uses
// a 10s axios timeout for the same call; Clyde matches it so a slow profile
// lookup never stalls the harvest loop.
const profileTimeout = 10 * time.Second

// ErrAccountUnidentifiable is returned by AccountIdentity when neither the
// profile endpoint nor any cached token-exchange account data yields an account
// uuid. The rotation layer treats it as a signal to skip storing the account
// rather than collapsing distinct accounts under an empty key.
var ErrAccountUnidentifiable = errors.New("anthropic: cannot resolve oauth account identity")

// profileResponse is the subset of the Anthropic OAuth profile response Clyde
// consumes. The Claude CLI reads account.uuid as the account id, account.email
// as the human label, account.display_name as an alternate label, and
// organization.uuid for org scoping (src/services/oauth/client.ts). Clyde keeps
// only the fields it uses; unknown fields are ignored by encoding/json.
type profileResponse struct {
	Account      profileAccount      `json:"account"`
	Organization profileOrganization `json:"organization"`
}

// profileAccount carries the per-account identity fields from the profile body.
type profileAccount struct {
	UUID        string `json:"uuid"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// profileOrganization carries the organization identity from the profile body.
type profileOrganization struct {
	UUID string `json:"uuid"`
}

// tokenExchangeAccount mirrors the optional account block the token endpoint
// returns alongside refreshed tokens (src/services/oauth/client.ts builds
// tokenAccount from data.account). It uses the email_address field name the
// token endpoint emits, which differs from the profile endpoint's email field.
type tokenExchangeAccount struct {
	UUID         string `json:"uuid"`
	EmailAddress string `json:"email_address"`
}

// cachedIdentity is one resolved identity plus the wall-clock instant it was
// resolved, so the cache can expire stale entries.
type cachedIdentity struct {
	identity provider.AccountIdentity
	resolved time.Time
}

// identityCacheTTL bounds how long a resolved identity is reused without
// re-hitting the profile endpoint. Anthropic OAuth access tokens are stable for
// hours, so a short TTL keeps the harvest loop (which ticks every few minutes)
// from calling /api/oauth/profile on every pass while still re-resolving well
// within a token's lifetime.
const identityCacheTTL = 1 * time.Hour

// identityCache memoizes resolved identities keyed by access token. The harvest
// loop resolves the same token on every tick, so without a cache it would call
// the profile endpoint repeatedly; the cache collapses those to one call per
// token per TTL. Access is guarded by mu because harvest and refresh run on
// separate goroutines.
type identityCache struct {
	mu      sync.Mutex
	entries map[string]cachedIdentity
}

func newIdentityCache() *identityCache {
	return &identityCache{mu: sync.Mutex{}, entries: make(map[string]cachedIdentity)}
}

// get returns a cached identity for an access token when present and unexpired.
func (c *identityCache) get(accessToken string, now time.Time) (provider.AccountIdentity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[accessToken]
	if !ok {
		return provider.AccountIdentity{Account: "", Label: ""}, false
	}
	if now.Sub(entry.resolved) > identityCacheTTL {
		delete(c.entries, accessToken)
		return provider.AccountIdentity{Account: "", Label: ""}, false
	}
	return entry.identity, true
}

// put records a resolved identity for an access token.
func (c *identityCache) put(accessToken string, identity provider.AccountIdentity, now time.Time) {
	if accessToken == "" || identity.Account == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[accessToken] = cachedIdentity{identity: identity, resolved: now}
}

// AccountIdentity resolves an access token into the account it belongs to. It
// checks the per-token cache first, then calls the Anthropic OAuth profile
// endpoint with the token as a Bearer credential. The profile account.uuid
// becomes the AccountID and account.email (or display_name) becomes the Label.
// A profile failure falls back to any token-exchange account data the Refresh
// path cached for this token. When neither yields a uuid it returns
// ErrAccountUnidentifiable so the rotation layer skips the account instead of
// keying it under an empty id.
func (p *Provider) AccountIdentity(ctx context.Context, accessToken string) (provider.AccountIdentity, error) {
	log := providerLog.Logger()
	none := provider.AccountIdentity{Account: "", Label: ""}
	if strings.TrimSpace(accessToken) == "" {
		log.ErrorContext(ctx, "anthropic.oauth.identity_empty_token",
			"subcomponent", "oauth_rotation",
			"err", "account identity requires an access token",
		)
		return none, fmt.Errorf("anthropic: account identity requires an access token: %w", ErrAccountUnidentifiable)
	}

	now := p.now()
	if cached, ok := p.identities.get(accessToken, now); ok {
		return cached, nil
	}

	identity, profileErr := p.fetchProfileIdentity(ctx, accessToken)
	if profileErr == nil {
		p.identities.put(accessToken, identity, now)
		log.InfoContext(ctx, "anthropic.oauth.identity_resolved",
			"subcomponent", "oauth_rotation",
			"source", "profile",
			"account", string(identity.Account),
			"email", identity.Label,
		)
		return identity, nil
	}

	if fallback, ok := p.identities.get(accessToken, now); ok {
		return fallback, nil
	}

	log.ErrorContext(ctx, "anthropic.oauth.identity_unresolved",
		"subcomponent", "oauth_rotation",
		"err", profileErr.Error(),
	)
	return none, fmt.Errorf("anthropic: resolve account identity: %w: %w", profileErr, ErrAccountUnidentifiable)
}

// fetchProfileIdentity performs the profile GET and projects the response into a
// neutral AccountIdentity. It returns an error on a build, transport, non-200,
// decode, or missing-uuid failure so the caller can fall back. Each failure is
// logged at Warn because a profile failure is recoverable through the
// token-exchange fallback; the final unresolved case is logged at Error by the
// caller. The empty zero value returned alongside every error is spelled out
// explicitly to satisfy the exhaustruct gate.
func (p *Provider) fetchProfileIdentity(ctx context.Context, accessToken string) (provider.AccountIdentity, error) {
	log := providerLog.Logger()
	none := provider.AccountIdentity{Account: "", Label: ""}

	endpoint, err := p.profileEndpoint()
	if err != nil {
		return none, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		log.WarnContext(ctx, "anthropic.oauth.profile_request_build_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return none, fmt.Errorf("anthropic: build profile request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		log.WarnContext(ctx, "anthropic.oauth.profile_request_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return none, fmt.Errorf("anthropic: get profile: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.WarnContext(ctx, "anthropic.oauth.profile_response_read_failed",
			"subcomponent", "oauth_rotation",
			"status", resp.StatusCode,
			"err", err.Error(),
		)
		return none, fmt.Errorf("anthropic: read profile response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		log.WarnContext(ctx, "anthropic.oauth.profile_status_error",
			"subcomponent", "oauth_rotation",
			"status", resp.StatusCode,
			"body_bytes", len(body),
		)
		return none, fmt.Errorf("anthropic: profile request failed: %s: body_bytes=%d", resp.Status, len(body))
	}

	var decoded profileResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		log.WarnContext(ctx, "anthropic.oauth.profile_decode_failed",
			"subcomponent", "oauth_rotation",
			"status", resp.StatusCode,
			"err", err.Error(),
		)
		return none, fmt.Errorf("anthropic: decode profile response: %w", err)
	}
	uuid := strings.TrimSpace(decoded.Account.UUID)
	if uuid == "" {
		log.WarnContext(ctx, "anthropic.oauth.profile_missing_uuid",
			"subcomponent", "oauth_rotation",
			"status", resp.StatusCode,
		)
		return none, errors.New("anthropic: profile response missing account uuid")
	}
	return provider.AccountIdentity{
		Account: provider.AccountID(uuid),
		Label:   profileLabel(decoded.Account),
	}, nil
}

// profileLabel chooses the human label from a profile account, preferring the
// email and falling back to the display name.
func profileLabel(account profileAccount) string {
	if email := strings.TrimSpace(account.Email); email != "" {
		return email
	}
	return strings.TrimSpace(account.DisplayName)
}

// profileEndpoint derives the profile URL from the configured MessagesURL by
// reusing its scheme and host and replacing the path. The official Claude CLI
// builds the profile endpoint as `${BASE_API_URL}/api/oauth/profile` where
// BASE_API_URL is the same api.anthropic.com origin MessagesURL points at, so
// reusing the MessagesURL host keeps Clyde's request on the configured upstream
// without a separate hardcoded host.
func (p *Provider) profileEndpoint() (string, error) {
	log := providerLog.Logger()
	raw := strings.TrimSpace(p.oauthCfg.MessagesURL)
	if raw == "" {
		return "", errors.New("anthropic: cannot derive profile endpoint: messages_url is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		log.Error("anthropic.oauth.profile_endpoint_parse_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return "", fmt.Errorf("anthropic: parse messages_url for profile endpoint: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("anthropic: messages_url has no scheme or host for profile endpoint")
	}
	endpoint := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: profilePath}
	return endpoint.String(), nil
}

// seedIdentityFromTokenAccount records an identity derived from a token-exchange
// account block so a later AccountIdentity call can fall back to it when the
// profile endpoint is unavailable. It is a no-op when the block carries no uuid.
func (p *Provider) seedIdentityFromTokenAccount(accessToken string, account tokenExchangeAccount) {
	uuid := strings.TrimSpace(account.UUID)
	if uuid == "" {
		return
	}
	p.identities.put(accessToken, provider.AccountIdentity{
		Account: provider.AccountID(uuid),
		Label:   strings.TrimSpace(account.EmailAddress),
	}, p.now())
}
