//go:build !darwin

package oauthcredentials

import "context"

// Read is the non-darwin stub. The macOS keychain only exists on darwin, so
// the reader on every other platform reports the entry as absent without
// raising an error. Callers that explicitly route a Platform="darwin" read
// from a non-darwin process land here and treat the result as "no keychain
// credential available".
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
