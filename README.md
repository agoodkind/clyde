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
curl -fsSL https://raw.githubusercontent.com/agoodkind/clyde/main/install.sh \
  | bash -s -- --daemon --hooks --mcp
```

The installer changes only the components you select. Use `--daemon`, `--hooks`,
or `--mcp` alone or together. Use `--binary-only` to install the Clyde binary
without changing a service or client setting.

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

Conversation search and MCP export use the daemon. Terminal export reads local
provider artifacts and does not require the daemon.

Run `clyde uninstall --apply` to remove the daemon service, Clyde hooks, and
Clyde MCP registrations. Use `--daemon`, `--hooks`, `--mcp`, or `--binary` to
remove only selected components. The default preserves the binary plus Clyde
configuration, cache, state, logs, exports, credentials, provider data,
repositories, and LMS data. Running the command without `--apply` prints help
and changes nothing.

Reload a running daemon after local config changes:

```bash
clyde daemon reload
```

After a database format cut, `clyde daemon hard-reset --apply` deletes Clyde's local
database contents, derived conversation and metrics state, logs, hook state, and
runtime state. It stops and unregisters the native user service and its identified
workers, then registers the executable running the command. Configuration,
credentials, certificate authority material, exports, provider artifacts, and LMS
data remain intact.

Run the command with the daemon's existing XDG root overrides. It preserves those
overrides in the reinstalled service and rejects targets outside Clyde's roots or
overlapping protected data before teardown. If installation fails, the deleted
data stays deleted; correct the reported service error and rerun the command.

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
