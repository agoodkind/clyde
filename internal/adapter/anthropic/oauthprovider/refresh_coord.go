package oauthprovider

import (
	"context"
	"fmt"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/properlock"
	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

// refreshLockTimeout bounds how long the rotator waits for the cross-process
// lock before giving up. It matches the proper-lockfile default behavior so
// the wait is bounded but tolerant of a concurrent Claude Code refresh that
// takes a few seconds to land.
const refreshLockTimeout = 30 * time.Second

// AcquireRefreshLock takes the proper-lockfile-compatible directory lock
// adjacent to the Claude credentials directory so a concurrent Claude Code
// OAuth refresh is serialized with this one. The proper-lockfile npm package
// produces "<target>.lock" next to the target; Claude Code passes the Claude
// config dir as the target (see
// research/claude-code-source-code-full/src/utils/auth.ts
// `checkAndRefreshOAuthTokenIfNeededImpl`), so the resulting lock directory
// lives at "${credentialsDir}.lock".
func (p *Provider) AcquireRefreshLock(ctx context.Context) (func() error, error) {
	log := providerLog.Logger()
	release, err := properlock.Acquire(ctx, p.credentialsDir, properlock.Options{
		StaleThreshold:       0, // accept package default (10s)
		AcquireTimeout:       refreshLockTimeout,
		RetryWait:            0, // accept package default
		MtimeRefreshInterval: 0, // accept package default
		Now:                  p.now,
	})
	if err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.refresh_lock_failed",
			"subcomponent", "oauth_rotation",
			"credentials_dir", p.credentialsDir,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("anthropic: acquire refresh lock: %w", err)
	}
	return release, nil
}

// ReadUpstreamCredentials reads the keychain credential (on macOS) or the
// on-disk .credentials.json (off macOS) and projects it into the neutral
// Credentials view. A missing entry yields (zero, false, nil).
func (p *Provider) ReadUpstreamCredentials(ctx context.Context) (provider.Credentials, bool, error) {
	options := oauthcredentials.ReadOptions{
		CredentialsDir:  p.credentialsDir,
		KeychainService: p.oauthCfg.KeychainService,
		SecurityBinary:  securityBinary,
		Platform:        "",
		Now:             p.now(),
	}
	empty := provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""}
	results := oauthcredentials.ReadCandidates(ctx, options)
	for _, result := range results {
		if result.Err != nil {
			providerLog.Logger().WarnContext(ctx, "anthropic.oauth.read_upstream_failed",
				"subcomponent", "oauth_rotation",
				"source", string(result.Source),
				"err", result.Err.Error(),
			)
			return empty, false, result.Err
		}
		if !result.Present || result.Tokens == nil {
			continue
		}
		return credentialsFromTokens(result.Tokens.Clone()), true, nil
	}
	return empty, false, nil
}

// WriteUpstreamCredentials writes the credential JSON into the macOS
// keychain entry Claude Code reads. The bytes are the same EncodeStored
// shape the on-disk .credentials.json carries, so a Claude Code reader sees
// a fresh credential on its next access.
func (p *Provider) WriteUpstreamCredentials(ctx context.Context, c provider.Credentials) error {
	log := providerLog.Logger()
	encoded, err := p.EncodeStored(c)
	if err != nil {
		return err
	}
	if err := oauthcredentials.WriteKeychainCredentials(ctx, encoded); err != nil {
		log.ErrorContext(ctx, "anthropic.oauth.write_upstream_failed",
			"subcomponent", "oauth_rotation",
			"err", err.Error(),
		)
		return fmt.Errorf("anthropic: write upstream credentials: %w", err)
	}
	return nil
}

// Compile-time assertion that Provider satisfies the optional rotation
// coordination contract.
var _ provider.RefreshCoordinator = (*Provider)(nil)
