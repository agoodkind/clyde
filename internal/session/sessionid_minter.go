package session

import "sync"

// sessionIDMinters maps a provider id to a function that mints a fresh provider
// session id before the upstream tool launches. Providers that pre-assign ids
// (Claude, whose id doubles as the transcript filename) register here at init;
// providers whose tool assigns its own id (Codex) do not register.
//
// This registry exists so the daemon can mint an id without importing the
// provider lifecycle packages. The Claude lifecycle imports the daemon, so the
// daemon importing the registry that pulls in the lifecycle would be an import
// cycle. Registration at init keeps the id-minting rule inside the provider
// package while letting the generic daemon dispatch by provider id.
var sessionIDMinters sync.Map

// RegisterSessionIDMinter associates a provider id with its pre-launch id minter.
// A later registration for the same provider replaces the previous one, which
// keeps test fakes simple.
func RegisterSessionIDMinter(providerID ProviderID, mint func() (string, error)) {
	sessionIDMinters.Store(NormalizeProviderID(providerID), mint)
}

// MintSessionID returns a freshly minted provider session id for providerID.
// ok is false when no minter is registered for the provider, meaning the
// provider does not pre-assign ids and the caller should create the record
// without one. err is non-nil only when a registered minter failed.
func MintSessionID(providerID ProviderID) (id string, ok bool, err error) {
	value, loaded := sessionIDMinters.Load(NormalizeProviderID(providerID))
	if !loaded {
		return "", false, nil
	}
	mint, isFunc := value.(func() (string, error))
	if !isFunc || mint == nil {
		return "", false, nil
	}
	minted, mintErr := mint()
	if mintErr != nil {
		return "", true, mintErr
	}
	return minted, true, nil
}
