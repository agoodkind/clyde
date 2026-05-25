//go:build !darwin

package oauthcredentials

import (
	"context"
	"errors"
)

// errKeychainUnsupported is returned by the keychain writer on every
// non-darwin platform. The keychain is a macOS facility; on other platforms
// the rotator routes credential persistence to the on-disk
// .credentials.json instead and never calls the keychain write path in
// production.
var errKeychainUnsupported = errors.New("keychain: macOS keychain is not available on this platform")

// writeEntry is the non-darwin stub. The function exists so the receiver
// method set matches across build tags; production callers on non-darwin
// platforms route around the keychain entirely.
func (w *keychainWriter) writeEntry(_ context.Context, _ string, _ []byte) error {
	return errKeychainUnsupported
}

// readExistingAccount is the non-darwin stub. Same rationale as writeEntry.
func (w *keychainWriter) readExistingAccount(_ context.Context) (string, error) {
	return "", errKeychainUnsupported
}
