# Clyde

Clyde is a local Go CLI and daemon for reading raw provider artifacts, exposing
conversation search and transcript export through CLI and MCP surfaces, hosting
adapter ingress, and capturing provider traffic through daemon-owned MITM
listeners.

Use Clyde for provider-owned artifact inspection. Use raw `claude` and `codex`
for provider session lifecycle and interactive work.

## Current References

Clyde moves quickly, so this README intentionally avoids copied command tables,
config schemas, route inventories, model catalogs, and listener lists. Use the
generated help or the focused documentation for details that can drift:

- CLI commands: `clyde --help` and `clyde <command> --help`.
- Conversation model, IDs, indexing, compaction segments, and export selection:
  [conversations](docs/conversations.md).
- Conversation CLI and MCP operations: `clyde conversation --help`.
- Runtime config: start from the [example configuration](clyde.example.toml).
- Adapter model routing and catalog behavior:
  [adapter model routing](docs/adapter/overview.md).
- Adapter Responses API (`/v1/responses`) and per-provider compatibility
  warnings: [adapter compatibility warnings](docs/adapter/compatibility.md).
- Cursor ingress and error behavior: [Cursor](docs/cursor.md).
- MITM listeners and capture behavior: [wire baseline](docs/wire-baseline.md)
  and [Cursor MITM setup](docs/cursor-mitm-setup.md).
- Logging, sinks, request paths, and inventory: [logging](docs/logging/).

The root command routes the CLI. One conversation-operation registry renders
both CLI and MCP surfaces, and alignment tests keep them consistent.

## Installation

```bash
curl -fsSL https://raw.githubusercontent.com/agoodkind/clyde/main/install.sh | bash
```

## Configuration

The common user config path is:

```text
~/.config/clyde/config.toml
```

Copy only the sections you need for your adapter, logging, search, and MITM
setup.

External gRPC clients can read `[daemon] grpc_address`; it defaults to `unix://`
plus the user-scoped daemon socket path.

## Operations

Use generated help to discover the current operational surface:

```bash
clyde --help
clyde conversation --help
clyde daemon --help
clyde logs --help
clyde mitm --help
```

Reload a running daemon after local config changes:

```bash
clyde daemon reload
```

State, logs, caches, adapter records, and MITM captures follow Clyde's XDG path
resolution. Use `clyde logs --help` for current paths and retention behavior.

Use [daemon metrics history](docs/logging/metrics.md) to inspect retained adapter activity with `clyde daemon status --since 1h`.

Try a change against a real daemon without disturbing the deployed one:

```bash
clyde daemon sandbox
```

It runs one daemon on throwaway directories with every listener disabled, and
ends when the command ends. See [docs/testing/overview.md](docs/testing/overview.md).

## Development

Common checks and build steps are Makefile-owned. Start with:

```bash
make build
make test
make lint
```

Use `make deploy` when the local daemon install and reload path needs to be
validated.

## Original Credit

Clyde is forked from Fabio Rehm's original
[clotilde](https://github.com/fgrehm/clotilde) project.

## License

This project is licensed under the MIT License. See `LICENSE`.
