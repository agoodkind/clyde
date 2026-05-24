package oauthprovider

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/providers/claude/oauthcredentials"
)

// securityBinary is the macOS keychain tool the underlying oauthcredentials
// keychain store shells out to. It matches the legacy adapter OAuth manager
// default so keychain reads behave identically after the rotation cutover.
const securityBinary = "security"

// readMirrorPlatformDarwin is the platform value that makes the
// oauthcredentials reader select its keychain store. A keychain MirrorSource
// is meaningful only on macOS; passing this explicitly lets ReadMirror route a
// "keychain" source to the keychain store regardless of the host platform the
// rest of the process observes.
const readMirrorPlatformDarwin = "darwin"

// readMirrorPlatformFile is any non-darwin platform value, used to force the
// oauthcredentials reader onto its file store for a "file" MirrorSource.
const readMirrorPlatformFile = "linux"

// ReadMirror reads one upstream MirrorSource through the existing Claude
// oauthcredentials reader and projects the result into the neutral Credentials
// view. A "file" source reads source.Location as the credentials directory's
// .credentials.json; a "keychain" source reads the configured keychain service
// item via `security find-generic-password`. A missing file or an empty
// keychain item returns present=false with no error; a genuine read or parse
// failure returns a non-nil error. The generic mirror calls this so it never
// reads a file or shells a keychain itself.
func (p *Provider) ReadMirror(ctx context.Context, src provider.MirrorSource) (provider.Credentials, bool, error) {
	empty := provider.Credentials{AccessToken: "", RefreshToken: "", ExpiresAt: time.Time{}, Raw: nil, Fingerprint: ""}
	options, source, err := p.readOptionsForSource(src)
	if err != nil {
		providerLog.Logger().ErrorContext(ctx, "anthropic.oauth.read_mirror_unknown_kind",
			"subcomponent", "oauth_rotation",
			"kind", src.Kind,
			"err", err.Error(),
		)
		return empty, false, err
	}
	results := oauthcredentials.ReadCandidates(ctx, options)
	for _, result := range results {
		if result.Source != source {
			continue
		}
		if result.Err != nil {
			providerLog.Logger().ErrorContext(ctx, "anthropic.oauth.read_mirror_failed",
				"subcomponent", "oauth_rotation",
				"kind", src.Kind,
				"source", string(result.Source),
				"err", result.Err.Error(),
			)
			// result.Err already carries store-level operation context
			// (the credential store wraps the underlying read with %w); the
			// generic mirror re-wraps with the source kind. Returning it
			// directly keeps the single contextual wrap from the store.
			return empty, false, result.Err
		}
		if !result.Present || result.Tokens == nil {
			return empty, false, nil
		}
		return credentialsFromTokens(result.Tokens.Clone()), true, nil
	}
	return empty, false, nil
}

// readOptionsForSource builds the oauthcredentials.ReadOptions that route the
// reader to the store backing the given MirrorSource Kind, and returns the
// concrete Source the reader will tag its result with.
func (p *Provider) readOptionsForSource(src provider.MirrorSource) (oauthcredentials.ReadOptions, oauthcredentials.Source, error) {
	switch src.Kind {
	case provider.MirrorSourceKindFile:
		credentialsDir := filepath.Dir(src.Location)
		return oauthcredentials.ReadOptions{
			CredentialsDir:  credentialsDir,
			KeychainService: "",
			SecurityBinary:  securityBinary,
			Platform:        readMirrorPlatformFile,
			Now:             p.now(),
		}, oauthcredentials.SourceFile, nil
	case provider.MirrorSourceKindKeychain:
		return oauthcredentials.ReadOptions{
			CredentialsDir:  "",
			KeychainService: src.Location,
			SecurityBinary:  securityBinary,
			Platform:        readMirrorPlatformDarwin,
			Now:             p.now(),
		}, oauthcredentials.SourceKeychain, nil
	default:
		return oauthcredentials.ReadOptions{CredentialsDir: "", KeychainService: "", SecurityBinary: "", Platform: "", Now: time.Time{}}, "", fmt.Errorf("anthropic: unsupported mirror source kind %q", src.Kind)
	}
}
