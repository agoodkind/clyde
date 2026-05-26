# Account Selection

Selection is the rotator deciding which account's access token to return for an upstream request. The entry point is `Rotator.Token(ctx, provider)` in `internal/oauthrotation/rotator.go`. The adapter reaches it through a small per-provider shim that satisfies the Anthropic client's token-source interface.

`Token` does the following in order:

- Runs one harvest pass for the provider (see `harvest.md`).
- Drops throttle entries whose reset time has passed (see `rate-limit-throttle.md`).
- Partitions the provider's accounts into eligible, throttled, and re-auth-marked sets.
- Among eligible accounts, picks the one whose upstream rate-limit window resets soonest in the future. Spending down the credit that refreshes first keeps monthly utilization balanced across accounts.
- Ties on the reset time are broken by oldest `lastObservedAt` so an account that has not been touched recently gets a turn.
- An account with no observation yet (zero `lastResetAt`) sorts after every observed account, so a freshly added account does not displace an observed one until it has been used at least once.
- When no eligible account exists, returns one of the typed errors below.

Two conditions produce a typed error instead of a token, both defined in `internal/oauthrotation/errors.go`:

- `AllAccountsThrottledError` carries the provider, the soonest reset time across the throttled accounts, and the account that resets first. It is returned when every account is throttled.
- `NeedsReauthError` carries the provider and the account. It is returned when the only candidate account has a dead refresh credential and no usable account exists. An account is marked for re-auth when the provider reports that its refresh credential is dead.

The adapter maps these errors through its error boundary into the OpenAI-compatible response envelope. `AllAccountsThrottledError` becomes HTTP 400 with type `invalid_request_error` and code `upstream_rate_limited`, with the soonest reset time in the message. `NeedsReauthError` becomes HTTP 400 with type `invalid_request_error` and code `upstream_auth_failed`, with a message instructing the operator to run `clyde oauth login`.

The setting `switch_on_limit` under `[adapter.anthropic.oauth.accounts]` gates only the move to a different account on a rate-limit signal. When it is false, selection serves the primary account and does not switch on a throttle. When it is true, a throttle signal moves selection to the next non-throttled account.
