# Clyde

Clyde is a local Go CLI and long-running daemon for routing, observing, and managing LLM work from developer tools. It provides an OpenAI-compatible HTTP adapter for clients such as Cursor, keeps a Clyde-owned index of provider sessions, and wraps live sessions so they can be launched, resumed, compacted, inspected, and remotely controlled from one local command surface.

## Command Surface

The `cmd/clyde` entrypoint registers these Clyde commands:

```text
clyde
clyde compact ...
clyde daemon ...
clyde hook sessionstart
clyde mcp
clyde resume <name|uuid>
```

Argument routing also handles these forms:

- `clyde -r <name>` and `clyde --resume <name>` run `clyde resume <name>`.
- Bare `clyde -r`, `clyde --resume`, and `clyde resume` open the dashboard.
- `clyde <existing-directory>` opens the dashboard scoped to that workspace.
- `clyde exec ...`, `clyde api ...`, `clyde -p ...`, and `clyde --print ...` forward to the real `claude` binary.
- Unknown commands are passed to Cobra first, then forwarded to the real `claude` binary when Cobra reports an unknown command.

## OpenAI-Compatible Adapter

`clyde daemon` can host the adapter HTTP server. The adapter routes requests through the configured model registry and provider backends.

The HTTP server registers these routes:

```text
/healthz
/v1/models
/v1/chat/completions
/v1/completions
/v1/messages
/v1/messages/count_tokens
/
```

The adapter configuration lives in the global Clyde config:

```text
~/.config/clyde/config.toml
```

`clyde.example.toml` contains the repository's reference config shape for these adapter sections:

```text
[adapter]
[adapter.codex]
[adapter.openai_compat_passthrough]
[adapter.oauth]
[adapter.client_identity]
[adapter.logprobs]
[adapter.families.<family>]
```

Common daemon commands:

```bash
clyde daemon
clyde daemon reload
```

The adapter host defaults to loopback. Use `localhost` or `[::1]` for local adapter URLs.

## Session Management

Clyde stores session metadata in a Clyde-owned project directory and a global session index. The dashboard and `resume` command use that store to resolve session names and provider session IDs.

The dashboard opens with:

```bash
clyde
```

A workspace-scoped dashboard opens with:

```bash
clyde /path/to/workspace
```

A named session resumes with:

```bash
clyde resume auth-feature
clyde --resume auth-feature
clyde -r auth-feature
```

For Claude Code sessions, the provider runtime launches the real `claude` binary with the resolved session ID and session settings when available. Clyde also forwards Claude-native invocations through the real `claude` binary when the arguments are not Clyde-owned commands.

The SessionStart hook command is:

```text
clyde hook sessionstart
```

The hook is installed with:

```bash
make install-hook
```

The hook lets Clyde register session metadata during Claude Code startup, resume, clear, and compact hook events.

## Compaction

`clyde compact` works on a session transcript. It appends a compact boundary and a synthetic follow-up message while keeping the original transcript lines on disk.

Examples:

```bash
clyde compact my-session
clyde compact my-session 200k
clyde compact my-session --tools --thinking
clyde compact my-session --target 200k --apply
clyde compact my-session --undo
clyde compact my-session --list-backups
```

By default, `clyde compact` previews the plan. `--apply` appends the compact boundary and synthetic message. `--undo` restores the most recent applied backup for that session.

Useful flags:

- `--tools` strips tool-use and tool-result content.
- `--thinking` drops thinking and redacted-thinking content.
- `--images` replaces image blocks with text placeholders.
- `--chat` drops older chat turns while preserving the required trailing turn shape.
- `--all` enables all compactable content classes.
- `--type` accepts `tools`, `thinking`, `images`, `chat`, and `all` as a CSV list.
- `--target` sets a token target such as `200k`, `120000`, or `1.2m`.
- `--refresh` forces a fresh context probe.
- `--summarize=false` skips the recap step during `--apply`.

## MITM Capture Proxy

Clyde includes a local MITM capture proxy for provider request observability. The proxy listens on IPv6 loopback, routes supported provider traffic to upstream services, and writes append-only capture records to `capture.jsonl` under the configured capture directory.

MITM configuration lives in the global Clyde config under `[mitm]`:

```toml
[mitm]
enabled_default = true
providers = ["claude", "codex", "cursor"]

[logging.raw_capture]
enabled = false
```

`providers` is a provider set. Use explicit provider names such as `claude`, `codex`, and `cursor`, or use `["all"]` to capture every supported family. `capture_dir` is optional and defaults to Clyde's state directory under `mitm`. Raw sidecar payload files are controlled only by `logging.raw_capture.enabled`; normal capture indexes use filtered inline payload metadata.

When MITM is enabled for Claude, Clyde's Claude passthrough path injects `ANTHROPIC_BASE_URL` so forwarded Claude traffic uses the local proxy. The daemon also starts a daemon-owned MITM listener when `[mitm].enabled_default` is true.

The MITM package also owns launch profiles for supported upstream clients and daemon-owned baseline drift checks configured under `[mitm.drift]`.

## Remote Control Harness

Clyde includes a daemon-owned remote control harness for live LLM sessions. It is the runtime layer that lets Clyde start a session, keep track of its provider-owned identity, stream live state to clients, and deliver user input back into the running provider process.

The harness exposes provider-neutral live-session RPCs. Clients such as the dashboard ask the daemon to start, list, stream, send to, foreground, or stop a live session. The daemon then routes those requests to the provider-specific runtime instead of letting clients inspect provider files, sockets, or process details directly.

For Claude sessions, Clyde launches Claude through its wrapper with remote control enabled. The wrapper runs Claude inside a PTY, creates a per-session Unix injection socket, keeps local terminal input and output working for foreground sessions, and writes daemon-sent text into Claude's PTY stdin as if the user had typed it. Daemon-owned headless Claude sessions use the same injection path without attaching local terminal IO.

For Codex live sessions, the daemon owns the live runtime directly and stores the active runtime record in memory. The same live-session RPC surface sends turns, streams events, and stops Codex sessions through the provider runtime.

Foreground handoff is part of the harness. When a user opens a daemon-owned live session in an interactive terminal, the daemon issues a short-lived foreground lease, suspends the background runtime when the provider needs that, and restores the daemon-owned live state after the foreground process exits when restoration is supported.

Runtime files for live sessions, including Claude injection sockets, live session state, and foreground handoff data, live under the daemon runtime directory listed below.

## Installation

### Build From Source

```bash
git clone https://goodkind.io/clyde
cd clyde
make build
make install
```

`make install` copies the signed development binary to:

```text
~/.local/bin/clyde
```

### Install Hook

```bash
make install-hook
```

### macOS LaunchAgent

```bash
make install-launch-agent
```

The LaunchAgent runs the installed binary at `~/.local/bin/clyde`.

## Quick Start

```bash
make build
make install
make install-hook
clyde
```

For adapter and MITM setup, create or edit the global config, copy the relevant adapter sections from `clyde.example.toml`, add any `[mitm]` settings for local capture, then start or reload the daemon:

```bash
mkdir -p ~/.config/clyde
$EDITOR ~/.config/clyde/config.toml
clyde daemon reload
```

If no daemon is running, start one with:

```bash
clyde daemon
```

## Data Locations

Clyde keeps project session metadata beside the workspace and global runtime data in XDG locations.

Data locations:

- Project session metadata and settings: `<project>/.claude/clyde/`.
- Global session index: `~/.local/share/clyde/sessions/`, or `XDG_DATA_HOME`.
- Global config: `~/.config/clyde/config.toml`, or `XDG_CONFIG_HOME`.
- Logs, compaction backups, context cache, adapter logs, and MITM captures: `~/.local/state/clyde/`, or `XDG_STATE_HOME`.
- Daemon socket and live session runtime files: `$TMPDIR/clyde-<uid>/` on macOS, or `$XDG_RUNTIME_DIR/clyde/` when set.

Add project-local Clyde state to `.gitignore`:

```gitignore
.claude/clyde/
```

## Development

Requirements:

- Go 1.26.2 or newer.
- Make.

Common targets:

```bash
make build         # compile and signing-check without leaving a repo-local clyde binary
make test          # run tests
make test-watch    # tests in watch mode
make coverage      # coverage report
make fmt           # format code
make lint          # run linter
make deadcode      # check for unreachable functions
make install       # copy the signed binary to ~/.local/bin/clyde
```

Recommended local setup:

```bash
make setup-hooks
make install-hook
```

On macOS, grant Full Disk Access to the installed binary path when using daemon-managed discovery, transcripts, or remote control:

```text
~/.local/bin/clyde
```

## Original Credit

Clyde is forked from Fabio Rehm's original [clotilde](https://github.com/fgrehm/clotilde) project.

## License

This project is licensed under the MIT License. See `LICENSE`.
