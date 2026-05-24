package oauthrotation

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// ForgetResult reports the outcome of a Forget call: whether an account matched
// and, when it did, the resolved account id that was removed.
type ForgetResult struct {
	Forgotten bool
	Account   provider.AccountID
}

// Forget removes one account from a provider. idOrLabel is matched first
// against the account id and then against the recorded operator label. On a
// match the slot is dropped from the in-memory map and selection order, the
// throttle entry is cleared, the reauth mark is cleared, and the on-disk
// account directory is deleted. A non-match returns Forgotten=false with no
// error so the caller can report "no such account" without treating it as a
// failure.
func (r *Rotator) Forget(ctx context.Context, name provider.Name, idOrLabel string) (ForgetResult, error) {
	state, err := r.providerState(ctx, name)
	if err != nil {
		return ForgetResult{}, err
	}
	query := strings.TrimSpace(idOrLabel)
	if query == "" {
		return ForgetResult{}, fmt.Errorf("forget account for provider %q: empty id or label", name)
	}

	state.mu.Lock()
	account, matched := matchAccountLocked(state, query)
	if matched {
		delete(state.slots, account)
		state.order = slices.DeleteFunc(state.order, func(a provider.AccountID) bool {
			return a == account
		})
		delete(state.reauth, account)
	}
	state.mu.Unlock()

	if !matched {
		r.logger.InfoContext(ctx, "oauthrotation.forget.no_match",
			"component", "oauthrotation",
			"provider", string(name),
			"query", query,
		)
		return ForgetResult{Forgotten: false, Account: ""}, nil
	}

	if err := state.throttle.clear(ctx, account); err != nil {
		r.logger.WarnContext(ctx, "oauthrotation.forget.clear_throttle_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"err", err.Error(),
		)
	}

	dir := accountDir(name, account)
	if err := os.RemoveAll(dir); err != nil {
		r.logger.ErrorContext(ctx, "oauthrotation.forget.remove_dir_failed",
			"component", "oauthrotation",
			"provider", string(name),
			"account", string(account),
			"dir", dir,
			"err", err.Error(),
		)
		return ForgetResult{Forgotten: true, Account: account}, fmt.Errorf("remove account dir %s: %w", dir, err)
	}
	r.logger.InfoContext(ctx, "oauthrotation.forget.removed",
		"component", "oauthrotation",
		"provider", string(name),
		"account", string(account),
	)
	return ForgetResult{Forgotten: true, Account: account}, nil
}

// matchAccountLocked resolves a query to an account by exact account id first,
// then by exact label. The caller must hold state.mu.
func matchAccountLocked(state *providerState, query string) (provider.AccountID, bool) {
	if _, ok := state.slots[provider.AccountID(query)]; ok {
		return provider.AccountID(query), true
	}
	for _, account := range state.order {
		slot := state.slots[account]
		slot.mu.Lock()
		label := slot.label
		slot.mu.Unlock()
		if label != "" && label == query {
			return account, true
		}
	}
	return "", false
}
