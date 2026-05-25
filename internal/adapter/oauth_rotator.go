package adapter

import (
	"context"
	"errors"
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
//
// This adapter-owned construction path is the fallback the adapter server
// uses when the daemon did not inject its single process-wide rotator (see
// internal/adapter/server.go registerAnthropicProvider). It still ships
// because tests and any non-daemon caller of the adapter package rely on it,
// so a Load kick lives here for symmetry with buildDaemonOAuthRotator.
//
// ctx is the caller-owned context used to drive the background startup load
// goroutine. Cancellation aborts the load cleanly; the caller retains
// ownership of cancellation and this function neither cancels nor derives a
// new ctx.
func buildAnthropicRotator(ctx context.Context, oauthCfg config.AdapterOAuth, log *slog.Logger) *oauthrotation.Rotator {
	if log == nil {
		log = slog.Default()
	}
	rotator := oauthrotation.NewRotator(log)
	rotation := oauthCfg.Accounts.WithDefaults()
	rotator.SetRefreshSafetyWindow(rotation.RefreshSafetyWindow.AsDuration())
	rotator.Register(oauthprovider.New(oauthCfg, ""))
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WarnContext(ctx, "oauthrotation.startup.load_panicked",
					"component", "oauthrotation",
					"provider", string(anthropicProviderName),
					"panic", r,
				)
			}
		}()
		if err := rotator.Load(ctx, anthropicProviderName); err != nil {
			log.WarnContext(ctx, "oauthrotation.startup.load_failed",
				"component", "oauthrotation",
				"provider", string(anthropicProviderName),
				"err", err.Error(),
			)
		}
	}()
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

// TokenAfterAuthFailure forwards the recovery call to the rotator, which
// either re-imports a live keychain credential (for an in-use account) or
// runs the refresh path (for an account Clyde owns). The rotator returns
// oauthrotation.ErrTokenUnchanged when the recovered token equals failedToken;
// the shim passes that sentinel through so the anthropic client can skip a
// pointless retry.
func (s *rotatorTokenSource) TokenAfterAuthFailure(ctx context.Context, failedToken string) (string, error) {
	token, err := s.rotator.TokenAfterAuthFailure(ctx, s.name, failedToken)
	if err != nil {
		if errors.Is(err, oauthrotation.ErrTokenUnchanged) {
			return token, fmt.Errorf("oauth rotation recover for %q: %w", s.name, err)
		}
		wrapped := fmt.Errorf("oauth rotation recover for %q: %w", s.name, err)
		s.log.ErrorContext(ctx, "adapter.oauth.recover_failed",
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
