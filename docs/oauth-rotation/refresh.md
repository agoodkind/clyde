# Token Refresh

An OAuth access token is short-lived. The rotator renews an account's access token by exchanging its refresh token for a new credential set, through the provider contract method `Refresh`. The refresh logic lives in `Rotator.RefreshDue` in `internal/oauthrotation/rotator.go`.

Refresh is expiry-driven. The daemon refresh loop in `internal/cli/daemon/loops.go` runs on the `refresh_interval` cadence under `[adapter.anthropic.oauth.accounts]`. On each tick, and once at daemon start, the loop checks every account and refreshes an account only when the current time plus the `refresh_safety_window` reaches that account's stored access-token expiry. The `refresh_safety_window` defaults to one hour. An account whose token is still well within its lifetime is skipped, so a daemon restart over a healthy set of accounts performs no refresh.

Each account refreshes under its own file lock, and the new credential is written to that account's `.credentials.json`. The stored expiry time is the source of truth across restarts, so the loop reads persisted expiry and skips accounts that are still valid.

Anthropic returns a new refresh token on every refresh and invalidates the prior refresh token. Clyde stores the new refresh token in its per-account file. Clyde does not write the refreshed credential back to the macOS keychain or to `~/.claude/.credentials.json`, so a separate tool reading those locations holds the prior refresh token after Clyde refreshes that account. A Clyde-launched `claude` avoids this because it reads the rotator credential through a dedicated config directory (see `launch-credentials.md`).
