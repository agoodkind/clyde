// Package oauthprovider implements the provider-agnostic OAuth rotation
// contract (internal/oauthrotation/provider.Provider) for Anthropic. It is a
// thin plug-in that delegates to the existing Claude OAuth code: credential
// read/write/round-trip lives in internal/providers/claude/oauthcredentials and
// account identity in internal/adapter/anthropic. This package owns the
// Anthropic-specific wire shapes and the refresh and login flows; the rotation
// layer stays provider-neutral.
package oauthprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

// providerName is the stable rotation-store identifier for Anthropic. It
// matches the path segment the rotation store keys accounts under.
const providerName provider.Name = "anthropic"

// credentialsFileName is the Claude Code on-disk credential file name, relative
// to the credentials directory.
const credentialsFileName = ".credentials.json"

// refreshTimeout caps a single token-endpoint round trip.
const refreshTimeout = 30 * time.Second

// compile-time assertion that Provider satisfies the rotation contract.
var _ provider.Provider = (*Provider)(nil)

// Provider implements provider.Provider for Anthropic by delegating to the
// existing Claude OAuth code paths.
type Provider struct {
	oauthCfg       config.AdapterOAuth
	credentialsDir string
	httpClient     *http.Client
	now            nowFunc
}

// New builds an Anthropic OAuth rotation plug-in. oauthCfg supplies the token
// URL, client id, scopes, and keychain service. credentialsDir is the Claude
// Code credentials directory used to locate the on-disk mirror source; it
// defaults to $HOME/.claude when empty.
func New(oauthCfg config.AdapterOAuth, credentialsDir string) *Provider {
	if credentialsDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			credentialsDir = filepath.Join(home, ".claude")
		}
	}
	return &Provider{
		oauthCfg:       oauthCfg,
		credentialsDir: credentialsDir,
		httpClient:     &http.Client{Timeout: refreshTimeout},
		now:            time.Now,
	}
}

// Name returns the stable Anthropic provider identifier.
func (p *Provider) Name() provider.Name {
	return providerName
}

// AccountID derives the account identity for an access token by extracting the
// account uuid via anthropic.AccountUUIDFromAccessToken. Anthropic OAuth tokens
// are opaque (sk-ant-oat...), so this returns an empty AccountID for the
// real-world case; it is exact for JWT-shaped tokens.
func (p *Provider) AccountID(accessToken string) (provider.AccountID, error) {
	return provider.AccountID(anthropic.AccountUUIDFromAccessToken(accessToken)), nil
}

// MirrorSources lists the two upstream Claude Code credential locations the
// rotation layer imports from: the on-disk .credentials.json file and the
// macOS keychain item named by the configured keychain service.
func (p *Provider) MirrorSources(_ context.Context) ([]provider.MirrorSource, error) {
	sources := []provider.MirrorSource{
		{
			Kind:     provider.MirrorSourceKindFile,
			Location: filepath.Join(p.credentialsDir, credentialsFileName),
		},
		{
			Kind:     provider.MirrorSourceKindKeychain,
			Location: p.oauthCfg.KeychainService,
		},
	}
	return sources, nil
}

// ParseStored decodes the Claude .credentials.json document shape into the
// neutral Credentials view. It delegates JSON handling to oauthcredentials and
// keeps the raw bytes for round-tripping.
func (p *Provider) ParseStored(raw []byte) (provider.Credentials, error) {
	log := providerLog.Logger()
	var document oauthcredentials.Document
	if err := json.Unmarshal(raw, &document); err != nil {
		log.Error("anthropic.oauth.parse_stored_failed",
			"subcomponent", "oauth_rotation",
			"reason", "malformed_document",
			"err", err.Error(),
		)
		return provider.Credentials{}, fmt.Errorf("anthropic: parse stored credentials: %w", err)
	}
	if document.ClaudeAIOauth == nil {
		log.Error("anthropic.oauth.parse_stored_failed",
			"subcomponent", "oauth_rotation",
			"reason", "missing_claude_ai_oauth_entry",
			"err", "no claudeAiOauth entry",
		)
		return provider.Credentials{}, errors.New("anthropic: parse stored credentials: no claudeAiOauth entry")
	}
	return credentialsFromTokens(document.ClaudeAIOauth.Clone()), nil
}

// EncodeStored encodes neutral Credentials back into the Claude
// .credentials.json document shape using the typed oauthcredentials.Document.
func (p *Provider) EncodeStored(c provider.Credentials) ([]byte, error) {
	log := providerLog.Logger()
	tokens := tokensFromCredentials(c)
	encoded, err := json.MarshalIndent(oauthcredentials.Document{ClaudeAIOauth: tokens}, "", "  ")
	if err != nil {
		log.Error("anthropic.oauth.encode_stored_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return nil, fmt.Errorf("anthropic: encode stored credentials: %w", err)
	}
	return encoded, nil
}

// credentialsFromTokens projects Claude Tokens into the neutral Credentials
// view, populating Fingerprint and ExpiresAt and re-encoding Raw so the stored
// bytes round-trip. A marshal error yields nil Raw; callers re-derive Raw via
// EncodeStored when they persist.
func credentialsFromTokens(tokens *oauthcredentials.Tokens) provider.Credentials {
	raw, marshalErr := json.MarshalIndent(oauthcredentials.Document{ClaudeAIOauth: tokens}, "", "  ")
	if marshalErr != nil {
		providerLog.Logger().Error("anthropic.oauth.credentials_raw_marshal_failed",
			"subcomponent", "oauth_rotation",
			"err", marshalErr.Error(),
		)
		raw = nil
	}
	expiresAt := time.Time{}
	if tokens.ExpiresAt != 0 {
		expiresAt = time.UnixMilli(tokens.ExpiresAt)
	}
	return provider.Credentials{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    expiresAt,
		Raw:          raw,
		Fingerprint:  oauthcredentials.Fingerprint(tokens),
	}
}

// tokensFromCredentials projects neutral Credentials back into Claude Tokens.
// When Raw carries a previously stored document it is decoded first so
// provider-only fields (scopes, subscriptionType, rateLimitTier) survive a
// round trip; the access, refresh, and expiry fields from Credentials take
// precedence.
func tokensFromCredentials(c provider.Credentials) *oauthcredentials.Tokens {
	tokens := &oauthcredentials.Tokens{
		AccessToken:      "",
		RefreshToken:     "",
		ExpiresAt:        0,
		Scopes:           nil,
		SubscriptionType: "",
		RateLimitTier:    "",
	}
	if len(c.Raw) > 0 {
		var document oauthcredentials.Document
		if err := json.Unmarshal(c.Raw, &document); err == nil && document.ClaudeAIOauth != nil {
			tokens = document.ClaudeAIOauth.Clone()
		}
	}
	tokens.AccessToken = c.AccessToken
	tokens.RefreshToken = c.RefreshToken
	if c.ExpiresAt.IsZero() {
		tokens.ExpiresAt = 0
	} else {
		tokens.ExpiresAt = c.ExpiresAt.UnixMilli()
	}
	return tokens
}
