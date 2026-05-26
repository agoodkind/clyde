# Launch Credentials

When Clyde launches a `claude` session, it can point that `claude` at the rotator's selected account instead of the operator's own keychain login. The setting `set_claude_config_dir` under `[adapter.anthropic.oauth.accounts]` controls this, and it defaults to off. It takes effect only when the adapter direct-OAuth path is enabled (`direct_oauth = true` under `[adapter]`).

## Adapter base URL

Claude Code reads `ANTHROPIC_BASE_URL` to choose where to send Anthropic API calls. The operator's shell rc sets the variable for shell-launched sessions. The daemon does not read the operator's shell rc, so daemon-spawned remote-control sessions would otherwise inherit no value and call Anthropic directly. The launch helper at `internal/providers/claude/lifecycle/invoke.go` fills the variable with the adapter loopback default `http://localhost:11434` when neither the caller env nor the daemon-supplied launch env carries a value. A caller-supplied or daemon-supplied value (including the MITM proxy URL) wins. The helper emits the `claude.launch.anthropic_base_url.default_applied` slog event on the `process.daemon.lifecycle` concern when the default is filled in, so the operator can confirm the redirect through the adapter is in place.

The mechanism uses the Claude tool's own credential resolution. On macOS the Claude tool reads its login from the macOS keychain first and reads `<CLAUDE_CONFIG_DIR>/.credentials.json` only when the keychain entry for that config directory is absent. The keychain entry name includes a hash of `CLAUDE_CONFIG_DIR` and is empty for any non-default directory. Pointing `CLAUDE_CONFIG_DIR` at a fresh directory therefore produces a keychain miss, and the Claude tool reads the file Clyde planted there.

When the setting is on, the daemon selects the rotator account, creates a scratch directory under the daemon runtime directory, writes the account's credential to `<scratch>/.credentials.json` with mode 0600, and returns `CLAUDE_CONFIG_DIR=<scratch>` in the launched process environment. The launch code lives in `internal/daemon/launch_credentials.go`, and the environment is applied in `internal/providers/claude/lifecycle/invoke.go`. The scratch directory is removed when the session ends, and the daemon removes any remaining scratch directories on shutdown.

The daemon writes only to its own scratch directory. It never writes to the operator's real `~/.claude` or to the macOS keychain.

When the selected account needs re-auth or no account is usable, a launch attached to a terminal prompts the operator before starting with three choices: log in now, skip this launch, or remove the account. A launch with no terminal does not prompt.
