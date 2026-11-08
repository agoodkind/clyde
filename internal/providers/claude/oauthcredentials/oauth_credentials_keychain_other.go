//go:build !darwin

package oauthcredentials

import "context"

// Read reports an absent keychain entry on platforms other than macOS.
//
// This stub does not query a keychain or return an error, including when the
// caller explicitly selects the darwin platform.
//
//	result := store.Read(ctx)
func (s keychainStore) Read(_ context.Context) ReadResult {
	return ReadResult{
		Source:  SourceKeychain,
		Tokens:  nil,
		Present: false,
		Err:     nil,
		Metadata: Metadata{
			AccessTokenPresent:  false,
			RefreshTokenPresent: false,
			ExpiresAtPresent:    false,
			ExpiresAt:           0,
			Expired:             false,
			Scopes:              nil,
			Fingerprint:         "",
			FileMtime:           0,
		},
	}
}
