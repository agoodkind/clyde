package oauthcredentials

import (
	"time"
)

// keychainStore reads the macOS login keychain entry that Claude Code uses
// for its OAuth tokens. The struct shape is shared across platforms so the
// dispatcher in oauth_credentials.go can construct it without a build tag;
// the platform-specific Read method lives in
// oauth_credentials_keychain_darwin.go and the non-darwin stub lives in
// oauth_credentials_keychain_other.go.
type keychainStore struct {
	keychainService string
	now             time.Time
}

func (s keychainStore) Source() Source {
	return SourceKeychain
}
