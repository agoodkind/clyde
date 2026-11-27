//go:build !darwin

package oauthcredentials

import (
	"context"
	"errors"
)

var errKeychainRequiresMacOS = errors.New("antigravity Keychain credentials require macOS")

func runSecurity(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return nil, errKeychainRequiresMacOS
}
