package daemon

import (
	"context"
	"log/slog"
	"sync"

	"goodkind.io/clyde/internal/adapter/anthropic/oauthprovider"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/oauthrotation"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/slogger"
)

// daemonOAuthProviderName is the rotation-store provider key the Anthropic
// OAuth plug-in registers under. It matches the Name the Anthropic provider
// returns and the path segment the rotation store keys accounts under.
const daemonOAuthProviderName provider.Name = "anthropic"

// buildDaemonOAuthRotator constructs the single, process-wide OAuth rotation
// layer the daemon owns. The adapter's serve and throttle path and the daemon's
// periodic harvest-and-refresh loop both run against this one instance so a
// token the refresh loop renews on disk is reflected in the adapter's in-memory
// account slot immediately, instead of only on the next mirror re-import. It
// returns nil when the adapter's direct-OAuth Anthropic path is disabled, in
// which case no rotator is needed.
//
// ctx is the daemon's long-lived context used to drive the background startup
// load. A reload-triggered cancellation aborts the load cleanly. The caller
// retains ownership of cancellation; this function neither cancels nor
// derives a new ctx.
func buildDaemonOAuthRotator(ctx context.Context, cfg config.AdapterConfig, log *slog.Logger) *oauthrotation.Rotator {
	if !cfg.DirectOAuth {
		return nil
	}
	rotatorLog := slogger.WithConcern(log.With("subcomponent", "oauth_rotation"), slogger.ConcernAdapterProviderAnthOAuth)
	rotator := oauthrotation.NewRotator(rotatorLog)
	rotation := cfg.Anthropic.OAuth.Accounts.WithDefaults()
	rotator.SetRefreshSafetyWindow(rotation.RefreshSafetyWindow.AsDuration())
	rotator.Register(oauthprovider.New(cfg.Anthropic.OAuth, ""))
	go func() {
		defer func() {
			if r := recover(); r != nil {
				rotatorLog.WarnContext(ctx, "oauthrotation.startup.load_panicked",
					"component", "oauthrotation",
					"provider", string(daemonOAuthProviderName),
					"panic", r,
				)
			}
		}()
		if err := rotator.Load(ctx, daemonOAuthProviderName); err != nil {
			rotatorLog.WarnContext(ctx, "oauthrotation.startup.load_failed",
				"component", "oauthrotation",
				"provider", string(daemonOAuthProviderName),
				"err", err.Error(),
			)
		}
	}()
	return rotator
}

// oauthRotatorHolder publishes the daemon's single OAuth rotation layer so the
// in-process harvest-and-refresh loop (an ExtraLoop in internal/cli/daemon,
// outside the daemon package's import graph) can drive the same instance the
// adapter serves from. The daemon populates it while assembling its long-lived
// subsystems, before the exclusive loops start; the loop reads it through
// [SharedOAuthRotator]. A package-level holder is used because ExtraLoop
// receives only a logger and cannot be handed the daemon-owned instance
// directly.
type oauthRotatorHolder struct {
	mu      sync.RWMutex
	rotator *oauthrotation.Rotator
}

var sharedOAuthRotator oauthRotatorHolder

// setSharedOAuthRotator records the daemon's single OAuth rotation layer.
func setSharedOAuthRotator(rotator *oauthrotation.Rotator) {
	sharedOAuthRotator.mu.Lock()
	defer sharedOAuthRotator.mu.Unlock()
	sharedOAuthRotator.rotator = rotator
}

// SharedOAuthRotator returns the daemon's single OAuth rotation layer, or nil
// when the adapter direct-OAuth path is disabled or the daemon has not finished
// assembling its subsystems. The in-process OAuth refresh loop calls this to run
// Harvest and RefreshAll against the same instance the adapter serves from.
func SharedOAuthRotator() *oauthrotation.Rotator {
	sharedOAuthRotator.mu.RLock()
	defer sharedOAuthRotator.mu.RUnlock()
	return sharedOAuthRotator.rotator
}
