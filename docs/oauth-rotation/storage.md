# Storage Layout

Clyde keeps one directory per account under the state directory. The state directory is `$XDG_STATE_HOME/clyde` when `XDG_STATE_HOME` is set, and `~/.local/state/clyde` otherwise. The path helpers live in `internal/oauthrotation/store_paths.go`.

The rotation store root is `<stateDir>/oauth`. Under it, each provider has its own subtree keyed by provider name (for example `anthropic`), so a second provider never collides with the first.

```
<stateDir>/oauth/<provider>/accounts/<account_id>/
    .credentials.json    # the account's tokens, file mode 0600
    .clyde-oauth.lock    # flock file guarding refresh and persist for this account
    .label               # optional operator-supplied label text, file mode 0600
<stateDir>/oauth/<provider>/throttle.json   # per-provider rate-limit ledger, file mode 0600
```

Directories are created with mode 0700. Credential files, the label file, and the throttle ledger are written with mode 0600. Only the daemon process reads or writes this tree; the `clyde oauth` commands reach it through daemon RPC.

For the Anthropic provider, `.credentials.json` uses the same JSON shape as `~/.claude/.credentials.json`, the file the Claude command-line tool writes. The `account_id` directory name is the Anthropic account UUID described in `account-identity.md`.
