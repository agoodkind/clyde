# Clyde

Clyde is a local Go CLI and daemon for reading raw Claude and Codex provider
artifacts, exposing conversation search and transcript export through CLI and
MCP surfaces, hosting adapter ingress, and capturing provider traffic through
daemon-owned MITM listeners.

Use Clyde for provider-owned artifact inspection. Use raw `claude` and `codex`
for provider session lifecycle and interactive work.

## Current References

Clyde moves quickly, so this README intentionally avoids copied command tables,
config schemas, route inventories, model catalogs, and listener lists. Use the
current source or generated help for details that can drift:

- CLI commands: `clyde --help` and `clyde <command> --help`.
- Conversation model, ids, indexing, compaction segments, and export selection:
  `docs/conversations.md`.
- Conversation CLI and MCP operations: `clyde conversation --help` and
  `internal/clispec/`.
- Runtime config: `clyde.example.toml` and `internal/config/`.
- Adapter model routing and catalog behavior:
  [adapter model routing](docs/adapter/overview.md).
- Cursor ingress and error behavior: `docs/cursor.md`.
- MITM listeners and capture behavior: `docs/wire-baseline.md`,
  `docs/cursor-mitm-setup.md`, and `internal/mitm/`.
- Logging, sinks, request paths, and inventory: `docs/logging/`.

`cmd/clyde/main.go` owns the root command routing. Conversation operations are
declared once in `internal/clispec/` and rendered onto both CLI and MCP
surfaces, with alignment covered by tests in that package.

## Installation

```bash
curl -fsSL https://raw.githubusercontent.com/agoodkind/clyde/main/install.sh | bash
```

## Configuration

The common user config path is:

```text
~/.config/clyde/config.toml
```

Copy only the sections you need from `clyde.example.toml`, then edit the local
config for your adapter, logging, search, and MITM setup. The config structs in
`internal/config/` are the implementation source of truth.

External gRPC clients can read `[daemon] grpc_address`; it defaults to `unix://`
plus the user-scoped daemon socket path from `internal/config`.

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
resolution. Use `clyde logs --help`, `docs/logging/`, and the config reference
for current paths and retention behavior.

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
