package oauthrotation

import (
	"context"
	"fmt"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// SelectForLaunch runs the same account selection as Token (mirror-sync pass,
// throttle-ledger load, stable walk that skips re-auth-marked and throttled
// slots) and returns the selected account along with its stored credential
// encoding. The bytes are the provider's EncodeStored form, identical to the
// per-account .credentials.json the rotator persists, so a launched provider
// process can authenticate as the currently-selected account by reading them.
//
// It is a pure read with no refresh side effects beyond the mirror sync Token
// already performs. The returned bytes carry token material; callers must treat
// them as secret (write 0600, never log contents) and must never write them
// back to the user's real provider config or keychain. When every account is
// throttled it returns AllAccountsThrottledError with the soonest reset.
func (r *Rotator) SelectForLaunch(ctx context.Context, name provider.Name) (provider.AccountID, []byte, error) {
	slot, account, err := r.selectActiveSlot(ctx, name)
	if err != nil {
		return "", nil, err
	}

	prov, err := r.providerImpl(name)
	if err != nil {
		return "", nil, err
	}

	slot.mu.Lock()
	cred := slot.cred
	slot.mu.Unlock()

	encoded, err := prov.EncodeStored(cred)
	if err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.launch.encode_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"err", err.Error(),
		)
		return "", nil, fmt.Errorf("encode stored credential for account %q: %w", account, err)
	}
	r.logger.DebugContext(ctx, "oauthrotation.launch.selected",
		"component", "oauthrotation",
		"provider", string(name),
		"account", string(account),
	)
	return account, encoded, nil
}

// providerImpl resolves the registered provider implementation for name so the
// launch-export path can re-encode the selected credential without holding the
// per-provider state lock across the encode.
func (r *Rotator) providerImpl(name provider.Name) (provider.Provider, error) {
	r.mu.RLock()
	state, ok := r.providers[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider %q not registered", name)
	}
	return state.prov, nil
}
