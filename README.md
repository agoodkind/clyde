# Clyde

Clyde is a local Go CLI and daemon for reading raw Claude and Codex provider
artifacts, exposing conversation search and transcript export through CLI and
MCP surfaces, hosting adapter ingress, and capturing provider traffic through
daemon-owned MITM listeners.

Clyde reads provider-owned artifacts. It does not create, resume, rename,
isolate, compact, wrap, or present provider sessions. Run `claude` and `codex`
directly for interactive work.

## Current References

Clyde moves quickly, so this README intentionally avoids copied command tables,
config schemas, route inventories, model catalogs, and listener lists. Use the
current source or generated help for details that can drift:

- CLI commands: `clyde --help` and `clyde <command> --help`.
- Conversation CLI and MCP operations: `clyde conversation --help` and
  `internal/clispec/`.
- Runtime config: `clyde.example.toml` and `internal/config/`.
- Adapter behavior: `docs/openai-adapter.md`, `docs/cursor.md`, and
  `internal/adapter/`.
- MITM listeners and capture behavior: `docs/mitm-listeners.md` and
  `internal/mitm/`.
- Logging, sinks, request paths, and inventory: `docs/SLOG.md` and
  `docs/logging/`.

`cmd/clyde/main.go` owns the root command routing. Conversation operations are
declared once in `internal/clispec/` and rendered onto both CLI and MCP
surfaces, with alignment covered by tests in that package.

## Installation

From a checkout:

```bash
make build
make install
```

Use the `Makefile` as the source of truth for exact build and install behavior.

## Configuration

The common user config path is:

```text
~/.config/clyde/config.toml
```

Copy only the sections you need from `clyde.example.toml`, then edit the local
config for your adapter, logging, search, and MITM setup. The config structs in
`internal/config/` are the implementation source of truth.

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
