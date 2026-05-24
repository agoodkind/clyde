package oauthrotation

import (
	"fmt"
	"time"

	"goodkind.io/clyde/internal/oauthrotation/provider"
)

// AllAccountsThrottledError is returned by the rotator when every registered
// account for a provider is currently throttled. SoonestReset is the earliest
// time any account is expected to clear, and Account is the account that holds
// that soonest reset.
//
// The name carries the linter-required Error suffix for error types; the task
// brief referred to this type as ErrAllAccountsThrottled, which the repo
// errname gate rejects for a type implementing error.
type AllAccountsThrottledError struct {
	Provider     provider.Name
	SoonestReset time.Time
	Account      provider.AccountID
}

// Error implements error.
func (e AllAccountsThrottledError) Error() string {
	return fmt.Sprintf(
		"oauthrotation: all accounts throttled for provider %q; soonest reset %s on account %q",
		e.Provider, e.SoonestReset.Format(time.RFC3339), e.Account,
	)
}

// NeedsReauthError is returned by the rotator when no usable account is
// available for a provider because one or more candidate accounts have a dead
// refresh credential and need a fresh login. Account names the account that
// holds the soonest path back into rotation through re-authentication. It is
// distinct from AllAccountsThrottledError: a throttled account recovers on its
// own at the reset time, whereas a re-auth account stays out of rotation until
// the operator logs in again, so the client-facing remedy differs.
type NeedsReauthError struct {
	Provider provider.Name
	Account  provider.AccountID
}

// Error implements error.
func (e NeedsReauthError) Error() string {
	return fmt.Sprintf(
		"oauthrotation: account %q for provider %q needs re-authentication",
		e.Account, e.Provider,
	)
}
