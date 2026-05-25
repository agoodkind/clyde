package oauthprovider

import (
	"log/slog"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/inuse"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

// AnthropicDetectorDeps carries the daemon-owned inputs the Anthropic in-use
// detector needs at registration time. AccountSnapshots returns the live
// rotator account fingerprints so a freshly imported account becomes
// matchable without restarting the daemon. DaemonPIDs lists the Claude
// worker PIDs the daemon currently owns so the detector excludes them from
// the external-session count. KeychainService is the macOS keychain entry
// name to read; an empty value disables keychain reads on every platform.
// Logger defaults to [slog.Default] when nil.
type AnthropicDetectorDeps struct {
	AccountSnapshots FingerprintSource
	DaemonPIDs       DaemonPIDSource
	KeychainService  string
	Logger           *slog.Logger
}

// RegisterAnthropicDetector builds the Anthropic InUseDetector from deps and
// registers it under the "anthropic" provider name in the process-wide
// inuse registry. The daemon calls this after it has both the rotator and a
// live PID accessor so the detector can compare keychain fingerprints
// against the rotator's slots and exclude daemon-owned workers from the
// external-process count. Passing an empty KeychainService is allowed; the
// resulting detector reads no keychain and always reports the empty set,
// which is the safe default for callers that have not configured a service
// name yet. The init pattern is not used here because the detector depends
// on daemon-owned state (rotator handle, livetrack PID accessor) that does
// not exist at package init time. Mirrors the per-provider setup pattern at
// internal/adapter/error_boundary_registration.go but as an explicit call
// rather than an init body.
func RegisterAnthropicDetector(deps AnthropicDetectorDeps) {
	detector := NewInUseDetector(InUseDeps{
		KeychainReader:   defaultKeychainReader(deps.KeychainService, time.Now),
		Fingerprint:      oauthcredentials.Fingerprint,
		DaemonPIDs:       deps.DaemonPIDs,
		AccountSnapshots: deps.AccountSnapshots,
		Now:              nil,
		Logger:           deps.Logger,
	})
	inuse.Register(provider.Name("anthropic"), detector)
}
