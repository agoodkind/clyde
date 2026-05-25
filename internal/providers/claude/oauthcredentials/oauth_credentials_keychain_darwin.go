//go:build darwin

package oauthcredentials

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/keybase/go-keychain"
)

// Read fetches the generic-password entry whose kSecAttrService matches
// s.keychainService and returns the parsed Document. A missing entry yields
// a result with Present=false and no error so callers can distinguish "not
// configured" from a genuine read failure.
func (s keychainStore) Read(_ context.Context) ReadResult {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(s.keychainService)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(true)

	results, err := keychain.QueryItem(query)
	if err != nil {
		if errors.Is(err, keychain.ErrorItemNotFound) {
			return emptyKeychainResult()
		}
		return ReadResult{
			Source:   SourceKeychain,
			Tokens:   nil,
			Present:  false,
			Err:      fmt.Errorf("keychain query %q: %w", s.keychainService, err),
			Metadata: emptyMetadata(),
		}
	}
	if len(results) == 0 {
		return emptyKeychainResult()
	}
	data := bytes.TrimSpace(results[0].Data)
	if len(data) == 0 {
		return emptyKeychainResult()
	}
	tokens, metadata, parseErr := parseBlob(data, s.now, 0)
	return ReadResult{
		Source:   SourceKeychain,
		Tokens:   tokens,
		Present:  tokens != nil,
		Err:      parseErr,
		Metadata: metadata,
	}
}

// emptyKeychainResult is the canonical "no credential" result. Centralizing
// it keeps every exhaustruct-required field set explicitly without repeating
// the zero literal at four call sites.
func emptyKeychainResult() ReadResult {
	return ReadResult{
		Source:   SourceKeychain,
		Tokens:   nil,
		Present:  false,
		Err:      nil,
		Metadata: emptyMetadata(),
	}
}

// emptyMetadata returns the zero Metadata with every field set explicitly.
// The package's struct-literal rule (exhaustruct) requires fully populated
// literals; centralizing the zero shape avoids repeating the field list at
// every keychain return site.
func emptyMetadata() Metadata {
	return Metadata{
		AccessTokenPresent:  false,
		RefreshTokenPresent: false,
		ExpiresAtPresent:    false,
		ExpiresAt:           0,
		Expired:             false,
		Scopes:              nil,
		Fingerprint:         "",
		FileMtime:           0,
	}
}
