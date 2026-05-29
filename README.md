# Clyde

Clyde is a local Go CLI and daemon for raw Claude and Codex transcript reading, MCP/CLI search, transcript export, adapter ingress, and MITM capture. Clyde does not create, resume, rename, isolate, compact, or present provider sessions. Run `claude` and `codex` directly for interactive work.

## Command Surface

The `cmd/clyde` entrypoint owns Clyde-specific commands only:

```text
clyde
clyde list-conversations
clyde get-conversation CONVERSATION_ID
clyde get-context CONVERSATION_ID
clyde search-conversation CONVERSATION_ID QUERY
clyde analyze-results RESULT_ID PROMPT
clyde export-transcript CONVERSATION_ID
clyde daemon ...
clyde logs ...
clyde mitm ...
clyde mcp
```

Running `clyde` with no arguments shows help. Unknown commands fail through Cobra. Clyde does not forward unknown arguments to provider CLIs.

## Conversations

Clyde scans provider-owned artifacts and derives stable conversation IDs without writing provider files:

```text
claude:<provider-session-id>
codex:<thread-id>
artifact:<path-hash>
```

The raw conversation index scans Claude artifacts under `~/.claude/projects` and Codex artifacts under the configured or default Codex roots. The daemon loads the last completed cache quickly at startup and refreshes the index in a debounced background worker. Stale cache data is acceptable while a refresh is running.

## MCP Tools

`clyde mcp` exposes the same conversation operations as the CLI:

```text
clyde_list_conversations
clyde_get_conversation
clyde_get_context
clyde_search_conversation
clyde_analyze_results
clyde_export_transcript
```

The tools use `conversation_id` arguments. There are no session-name aliases.

## Transcript Export

`clyde export-transcript` accepts a conversation ID and supports markdown, plain text, HTML, and JSON output. Export options include message filtering, thinking blocks, tool calls, tool outputs, raw JSON metadata, history start, and whitespace handling.

## Adapter

`clyde daemon` can host the OpenAI-compatible adapter. The adapter routes requests through the configured model registry and provider backends.

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

The adapter config lives in:

```text
~/.config/clyde/config.toml
```

`clyde.example.toml` contains the reference config shape for adapter, auth, logging, search, and MITM sections.

## Provider Auth

Clyde keeps provider credential handling behind a generic auth boundary. The daemon injects provider-specific readers for Anthropic/Claude and Codex. The Anthropic path reads the current Claude credential source for the local platform, including macOS keychain support and file-backed Linux support.

## MITM Capture Proxy

Clyde includes a local MITM capture proxy for provider request observability. The proxy listens on loopback, routes supported provider traffic to upstream services, and writes append-only capture records under the configured capture directory.

MITM configuration lives under `[mitm]`:

```toml
[mitm]
enabled_default = true
providers = ["claude", "codex", "cursor"]

[logging.raw_capture]
enabled = false
```

`providers` is a provider set. Use explicit provider names such as `claude`, `codex`, and `cursor`, or use `["all"]` to capture every supported family.

## Installation

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

## Quick Start

```bash
make build
make install
clyde list-conversations
clyde daemon
```

For adapter and MITM setup, create or edit the global config, copy the relevant sections from `clyde.example.toml`, then start or reload the daemon:

```bash
mkdir -p ~/.config/clyde
$EDITOR ~/.config/clyde/config.toml
clyde daemon reload
```

## Data Locations

Clyde keeps global config, caches, logs, adapter state, and MITM captures in XDG locations:

- Global config: `~/.config/clyde/config.toml`, or `XDG_CONFIG_HOME`.
- Conversation index cache, logs, adapter logs, and MITM captures: `~/.local/state/clyde/`, or `XDG_STATE_HOME`.
- Daemon socket and runtime files: `$TMPDIR/clyde-<uid>/` on macOS, or `$XDG_RUNTIME_DIR/clyde/` when set.

## Development

Common targets:

```bash
make build
make install
make deploy
```

## Original Credit

Clyde is forked from Fabio Rehm's original [clotilde](https://github.com/fgrehm/clotilde) project.

## License

This project is licensed under the MIT License. See `LICENSE`.
