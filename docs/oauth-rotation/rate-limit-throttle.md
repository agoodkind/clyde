# Rate-Limit Throttle

A throttle is a record that an account hit an Anthropic usage limit and should be skipped until a reset time. The rotator records throttles and the selection walk skips throttled accounts (see `selection.md`).

A provider reports a limit through the rate-limit sink contract in `internal/oauthrotation/ratelimitsink`. The contract defines a `Signal` carrying the provider name, the access token that was used, a claim naming the spent window, a reset time, an observation time, and the HTTP status. The claim is one of `five_hour`, `seven_day`, `seven_day_opus`, or `unknown`.

The Anthropic provider produces a signal in `internal/adapter/anthropic/ratelimit_signal.go`. It emits only on a hard limit: an HTTP 429 response, or an HTTP 200 response carrying the header `anthropic-ratelimit-unified-overage-status: rejected`. It maps the header `anthropic-ratelimit-unified-representative-claim` to the claim value, and reads the reset time from the unified reset headers. An unrecognized claim value maps to `unknown` and is logged once.

The rotator implements the sink. On a signal it finds the account by its access token, writes a throttle entry for that account, and logs the throttle. The entry records the reset time, the claim, the observation time, and the HTTP status.

Throttle entries persist per provider in `<stateDir>/oauth/<provider>/throttle.json`, locked with a file lock so concurrent writers are safe. The reset window can outlive the daemon, so the ledger is on disk rather than in memory. Entries whose reset time has passed are dropped when the ledger loads.
