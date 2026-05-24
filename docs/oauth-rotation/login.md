# Login and Account Management

The `clyde oauth` commands manage rotation accounts. They are thin gRPC clients to the daemon and do not read or write the credential store directly. The command definitions live in `cmd/oauth.go`.

- `clyde oauth login [--provider=anthropic] [--email=<addr>] [--label=<name>]` adds an account.
- `clyde oauth list [--provider=anthropic]` prints the accounts and their status.
- `clyde oauth forget [--provider=anthropic] <id|label>` removes an account.

Each command maps to one daemon RPC on the service in `api/clyde/v1/daemon/service.proto`: `OAuthAccountList`, `OAuthAccountLogin`, and `OAuthAccountForget`. `OAuthAccountLogin` is server-streaming: its first event carries the authorize URL and a later event carries the result.

The daemon runs the Anthropic login by spawning `claude auth login` through the provider contract methods `SpawnLogin` and `CompleteLogin`. The spawn sets the child's `HOME` to a throwaway scratch directory, sets `BROWSER=true`, and removes `open` and `xdg-open` from the child's `PATH`, so the Claude tool prints the sign-in URL instead of opening a browser. The daemon reads the URL from the child's output and streams it to the caller. The `clyde oauth login` command prints that URL and copies it to the clipboard, so the operator opens it in any browser session. When the child finishes, the daemon reads the credential the child wrote into the scratch directory, resolves the account identity, stores the account under its own directory, and removes the scratch directory.

`clyde oauth list` reports a status per account derived from the stored state: ready, throttled with the reset time, or needs re-auth. The same states drive the terminal user interface account view and status indicator.
