// Package oauthrotation owns Clyde's per-account OAuth credential store and the
// rotation logic that picks a non-throttled account, mirrors upstream
// credentials one-way, refreshes tokens, and records rate-limit throttles. It
// depends only on the provider contract and the rate-limit sink contract, and
// imports no provider package.
package oauthrotation

import (
	"path/filepath"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/oauthrotation/provider"
)

const (
	// oauthSubdir is the rotation store root under the clyde state dir.
	oauthSubdir = "oauth"
	// accountsSubdir holds one directory per account under a provider.
	accountsSubdir = "accounts"

	// credentialsFileName is the per-account credential file. Written 0600.
	credentialsFileName = ".credentials.json"
	// lockFileName is the per-account flock file guarding refresh and persist.
	lockFileName = ".clyde-oauth.lock"
	// labelFileName is the per-account operator label file. Written 0600.
	labelFileName = ".label"

	// credentialFileMode is the permission for credential files.
	credentialFileMode = 0o600
	// storeDirMode is the permission for store directories.
	storeDirMode = 0o700
)

// storeRoot returns the rotation store root: <stateDir>/clyde/oauth. It reuses
// config.DefaultStateDir, which resolves $XDG_STATE_HOME/clyde or
// ~/.local/state/clyde.
func storeRoot() string {
	return filepath.Join(config.DefaultStateDir(), oauthSubdir)
}

// providerDir returns the per-provider directory: <storeRoot>/<provider>.
func providerDir(name provider.Name) string {
	return filepath.Join(storeRoot(), string(name))
}

// accountsDir returns the accounts root for a provider:
// <storeRoot>/<provider>/accounts.
func accountsDir(name provider.Name) string {
	return filepath.Join(providerDir(name), accountsSubdir)
}

// accountDir returns one account's directory:
// <storeRoot>/<provider>/accounts/<account_id>.
func accountDir(name provider.Name, account provider.AccountID) string {
	return filepath.Join(accountsDir(name), string(account))
}

// accountCredentialsPath returns the per-account credentials file path.
func accountCredentialsPath(name provider.Name, account provider.AccountID) string {
	return filepath.Join(accountDir(name, account), credentialsFileName)
}

// accountLockPath returns the per-account flock path.
func accountLockPath(name provider.Name, account provider.AccountID) string {
	return filepath.Join(accountDir(name, account), lockFileName)
}

// accountLabelPath returns the per-account operator label file path.
func accountLabelPath(name provider.Name, account provider.AccountID) string {
	return filepath.Join(accountDir(name, account), labelFileName)
}
