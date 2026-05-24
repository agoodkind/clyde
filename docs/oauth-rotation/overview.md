# OAuth Rotation Overview

Clyde holds several Anthropic OAuth logins at once and sends one account's access token per upstream request. When the selected account hits an Anthropic usage limit, Clyde sends the next account that still has quota. When every account is limited, Clyde returns a typed error carrying the soonest reset time.

The rotation layer lives in the package `internal/oauthrotation` and depends only on two contracts: the provider contract in `internal/oauthrotation/provider` (what the rotator needs from an upstream) and the rate-limit sink contract in `internal/oauthrotation/ratelimitsink` (how a provider reports a limit). The rotation layer imports no provider package, so a second provider is a separate plugin.

Anthropic is the one provider implemented today. Its plugin lives in `internal/adapter/anthropic/oauthprovider` and supplies the Anthropic-specific credential format, account identity, token refresh, login spawn, and credential sources.

The daemon owns one rotator for the whole process. The daemon builds it in `internal/daemon/oauth_rotator.go` when the adapter's direct-OAuth path is enabled (`direct_oauth = true` under `[adapter]`). The adapter serve path and the daemon refresh loop use that same instance. The `clyde oauth` commands reach it over daemon RPC and never touch the credential files directly.

Read the focused references next:

- `storage.md` for the on-disk credential layout and file permissions.
- `harvest.md` for importing logins from the macOS keychain and the Claude credentials file.
- `account-identity.md` for how an account is keyed and labeled.
- `selection.md` for which account is chosen and the typed errors when none can serve.
- `refresh.md` for when tokens are renewed and the effect on a shared login.
- `rate-limit-throttle.md` for how an Anthropic usage limit demotes an account.
- `login.md` for adding an account through the CLI and the daemon login flow.
- `launch-credentials.md` for pointing a Clyde-launched `claude` at a rotator account.
- `config.md` for the configuration block and defaults.
- `provider-contract.md` for the interface a new provider implements.
- `logging.md` for the structured log events the rotation layer emits.
