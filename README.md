# Clyde

Clyde is a local Go CLI and daemon for reading raw Claude and Codex provider
artifacts, exposing conversation search and transcript export through CLI and
MCP surfaces, hosting adapter ingress, and capturing provider traffic through
daemon-owned MITM listeners.

## Current References

Clyde moves quickly, so this README intentionally avoids copied command tables,
config schemas, route inventories, model catalogs, and listener lists. Use the
current source or generated help for details that can drift:

- CLI commands: `clyde --help` and `clyde <command> --help`.
- Docs: [docs](docs).

## Configuration

The common user config path is:

```text
~/.config/clyde/config.toml
```

Copy only the sections you need from [clyde.example.toml](clyde.example.toml), then edit the local
config for your adapter, logging, search, and MITM setup.

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

This project is licensed under the MIT License. See [LICENSE](LICENSE).
