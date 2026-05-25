// Package inuse declares the generic contract the OAuth rotator uses to ask
// "is this account currently in use by an external process?" The contract is
// provider-neutral: this package owns the interface, the typed set, the
// process-wide registry, and a noop default. Provider-specific implementations
// live in the provider package and register themselves at startup so the
// rotator never imports a provider package or knows about keychain entries,
// process binaries, or fingerprint helpers.
package inuse

import (
	"context"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// Set is a typed set of account ids reported as in-use by a Detector. It
// is a named struct around a typed map so the contract does not leak loose
// map types into the rotator surface. The zero value is a valid empty set:
// Contains reports false for every account and Len reports zero.
type Set struct {
	accounts map[provider.AccountID]struct{}
}

// NewSet builds a Set seeded with zero or more accounts. The empty
// constructor argument list yields the "nothing in use" set.
func NewSet(accounts ...provider.AccountID) Set {
	set := Set{accounts: make(map[provider.AccountID]struct{}, len(accounts))}
	for _, account := range accounts {
		set.accounts[account] = struct{}{}
	}
	return set
}

// Contains reports whether account is in the set. A zero-value Set
// reports false for every account so callers can treat the noop detector's
// return identically to a real empty set.
func (s Set) Contains(account provider.AccountID) bool {
	if s.accounts == nil {
		return false
	}
	_, ok := s.accounts[account]
	return ok
}

// Len reports how many distinct accounts are marked in use.
func (s Set) Len() int {
	return len(s.accounts)
}

// Detector answers the in-use question for one provider. The rotator passes
// the candidate account ids for the current pass; the implementation may use
// them to narrow its work or ignore them and report the full in-use set.
// A nil-or-empty return value with a nil error is the safe default and means
// "no account is currently in use".
type Detector interface {
	Detect(ctx context.Context, accounts []provider.AccountID) (Set, error)
}
