# Configuration

OAuth rotation is configured under the Anthropic provider namespace. The account-rotation keys live in `[adapter.anthropic.oauth.accounts]`. The credential endpoints and client metadata live in `[adapter.anthropic.oauth]` and are described where they are used (see `account-identity.md` and `harvest.md`). The structs are defined in `internal/config/config.go`.

The rotation layer is active only when the adapter direct-OAuth path is enabled with `direct_oauth = true` under `[adapter]`. When it is active, the rotator always harvests, refreshes, and serves accounts; the keys below tune that behavior.

```toml
[adapter.anthropic.oauth.accounts]
switch_on_limit = true        # switch to another account when one hits its usage limit
scan_interval = "5m"          # how often to harvest logins from external sources
refresh_interval = "30m"      # how often to check accounts for a due refresh
refresh_safety_window = "1h"  # refresh an account only when its access token is within this window of expiry
set_claude_config_dir = true  # point a Clyde-launched claude at the selected rotator account
```

Keys and defaults:

- `switch_on_limit` is a boolean, default false. It gates only the move to a different account on a rate-limit signal (see `selection.md`). When false, selection serves the primary account and does not switch on a throttle.
- `scan_interval` is a duration string, default `5m`. It is the cadence at which the daemon imports logins from external sources (see `harvest.md`).
- `refresh_interval` is a duration string, default `30m`. It is the cadence at which the daemon checks accounts for a due refresh (see `refresh.md`).
- `refresh_safety_window` is a duration string, default `1h`. An account is refreshed only when the current time plus this window reaches the access token's expiry (see `refresh.md`).
- `set_claude_config_dir` is a boolean, default false. When true, a Clyde-launched `claude` reads the selected rotator account through a dedicated config directory (see `launch-credentials.md`).

Validation rejects a negative `scan_interval`, `refresh_interval`, or `refresh_safety_window`. A zero or unset duration takes the default.
