package adapter

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/adapter/anthropic/oauthprovider"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/oauthrotation"
	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// anthropicProviderName is the rotation-store provider key the Anthropic OAuth
// plug-in registers under. The adapter token shim keys its rotator lookups on
// it.
const anthropicProviderName provider.Name = "anthropic"

// buildAnthropicRotator constructs the OAuth rotation layer for the Anthropic
// direct-OAuth path and registers the Anthropic plug-in on it. The rotator
// harvests upstream Claude Code credentials, refreshes them, serves the access
// token, and records throttles; it implements [ratelimitsink.Sink] so the
// anthropic client can report rate-limit signals back to it. The returned
// rotator is shared as both the client's token source (through
// rotatorTokenSource) and its rate-limit sink.
func buildAnthropicRotator(oauthCfg config.AdapterOAuth, log *slog.Logger) *oauthrotation.Rotator {
	rotator := oauthrotation.NewRotator(log)
	rotator.Register(oauthprovider.New(oauthCfg, ""))
	return rotator
}

// rotatorTokenSource adapts a [oauthrotation.Rotator] to the anthropic client's
// [anthropic.OAuthSource]. It calls Token for the Anthropic provider and
// returns just the access token string, discarding the account id the client
// does not need on the request path.
type rotatorTokenSource struct {
	rotator *oauthrotation.Rotator
	name    provider.Name
	log     *slog.Logger
}

// newRotatorTokenSource builds the adapter-side shim over the rotator for the
// Anthropic provider.
func newRotatorTokenSource(rotator *oauthrotation.Rotator, log *slog.Logger) *rotatorTokenSource {
	if log == nil {
		log = slog.Default()
	}
	return &rotatorTokenSource{rotator: rotator, name: anthropicProviderName, log: log}
}

// Token returns the current non-throttled access token for the Anthropic
// provider, satisfying [anthropic.OAuthSource].
func (s *rotatorTokenSource) Token(ctx context.Context) (string, error) {
	token, _, err := s.rotator.Token(ctx, s.name)
	if err != nil {
		wrapped := fmt.Errorf("oauth rotation token for %q: %w", s.name, err)
		s.log.ErrorContext(ctx, "adapter.oauth.token_lookup_failed",
			"subcomponent", "oauth_rotation",
			"provider", string(s.name),
			"err", wrapped.Error(),
		)
		return "", wrapped
	}
	return token, nil
}

// compile-time assertion that the shim satisfies the client's token source.
var _ anthropic.OAuthSource = (*rotatorTokenSource)(nil)
