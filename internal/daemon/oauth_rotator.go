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
//
// mitmTracker reports the count of active claude-to-anthropic MITM sessions
// the in-use detector uses to decide whether the keychain-matched account is
// currently held by Claude Code. Passing nil disables the in-use detector
// entirely, which is the right default for callers that have not stood up
// the MITM proxy yet.
func buildDaemonOAuthRotator(ctx context.Context, cfg config.AdapterConfig, log *slog.Logger, mitmTracker oauthprovider.ClaudeMITMTracker) *oauthrotation.Rotator {
	if !cfg.DirectOAuth {
		return nil
	}
	rotatorLog := slogger.WithConcern(log.With("subcomponent", "oauth_rotation"), slogger.ConcernAdapterProviderAnthOAuth)
	rotator := oauthrotation.NewRotator(rotatorLog)
	rotation := cfg.Anthropic.OAuth.Accounts.WithDefaults()
	rotator.SetRefreshSafetyWindow(rotation.RefreshSafetyWindow.AsDuration())
	rotator.Register(oauthprovider.New(cfg.Anthropic.OAuth, ""))
	if mitmTracker != nil {
		oauthprovider.RegisterAnthropicDetector(oauthprovider.AnthropicDetectorDeps{
			AccountSnapshots: anthropicAccountSnapshots(ctx, rotator),
			MITMTracker:      mitmTracker,
			KeychainService:  cfg.Anthropic.OAuth.KeychainService,
			Logger:           rotatorLog,
		})
	}
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

// anthropicAccountSnapshots adapts the rotator's typed Accounts snapshot into
// the FingerprintSource shape the Anthropic detector consumes. The detector
// only needs the (account id, fingerprint) pairs from each snapshot; the
// rotator owns the full AccountSnapshot type so the projection lives here at
// the wiring boundary rather than in the provider package.
func anthropicAccountSnapshots(ctx context.Context, rotator *oauthrotation.Rotator) oauthprovider.FingerprintSource {
	return func() []oauthprovider.AccountFingerprint {
		snapshots, err := rotator.Accounts(ctx, daemonOAuthProviderName)
		if err != nil {
			return nil
		}
		out := make([]oauthprovider.AccountFingerprint, 0, len(snapshots))
		for _, snapshot := range snapshots {
			out = append(out, oauthprovider.AccountFingerprint{
				Account:     snapshot.Account,
				Fingerprint: snapshot.Fingerprint,
			})
		}
		return out
	}
}

// oauthRotatorHolder publishes the daemon's single OAuth rotation layer so the
// in-process harvest-and-refresh loop (an ExtraLoop in internal/cli/daemon,
// outside the daemon package's import graph) can drive the same instance the
// adapter serves from. The daemon populates it while assembling its long-lived
// subsystems, before the exclusive loops start; the loop reads it through
// [SharedOAuthRotator]. A package-level holder is used because ExtraLoop
// receives only a logger and cannot be handed the daemon-owned instance
// directly. The receiver field carries the daemon's downstream injector (the
// MITM proxy wiring) so the rotator setter can notify the proxy in the same
// critical section that publishes the shared instance.
type oauthRotatorHolder struct {
	mu       sync.RWMutex
	rotator  *oauthrotation.Rotator
	receiver func(*oauthrotation.Rotator)
}

var sharedOAuthRotator oauthRotatorHolder

// setSharedOAuthRotator records the daemon's single OAuth rotation layer and
// notifies any registered downstream receiver (typically the MITM proxy) so
// subsystems that need the live rotator instance see it as soon as it
// publishes. Receiver callbacks must be cheap and non-blocking; they run
// under the holder mutex.
func setSharedOAuthRotator(rotator *oauthrotation.Rotator) {
	sharedOAuthRotator.mu.Lock()
	defer sharedOAuthRotator.mu.Unlock()
	sharedOAuthRotator.rotator = rotator
	if sharedOAuthRotator.receiver != nil {
		sharedOAuthRotator.receiver(rotator)
	}
}

// registerOAuthRotatorReceiver records a single downstream consumer of the
// daemon's OAuth rotator. The receiver fires once at registration time with
// the current rotator (which may be nil if the daemon has not assembled the
// adapter yet) and again whenever setSharedOAuthRotator publishes a new
// instance. Used by the MITM proxy startup so the proxy can call
// SetRotatorSink as soon as the rotator exists, without enlarging
// startAdapter's wiring footprint in run.go.
func registerOAuthRotatorReceiver(receiver func(*oauthrotation.Rotator)) {
	sharedOAuthRotator.mu.Lock()
	defer sharedOAuthRotator.mu.Unlock()
	sharedOAuthRotator.receiver = receiver
	if receiver != nil {
		receiver(sharedOAuthRotator.rotator)
	}
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
