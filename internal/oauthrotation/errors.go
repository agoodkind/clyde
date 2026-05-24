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
