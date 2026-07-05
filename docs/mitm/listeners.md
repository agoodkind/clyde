# MITM listeners

This table mirrors the `[mitm.cli.*]` and `[mitm.app.*]` blocks in
`clyde.example.toml`, which is the source of truth for listener hosts and ports.
The live ports come from the operator config, so this table can drift; treat
`clyde.example.toml` as authoritative if they disagree.

Each listener is one loopback port for one application. Every exchange it
captures carries that listener id in the capture `client` column. The listener
captures every host reached on its port, not only provider-claimed hosts; see
[capture.md](capture.md) for how capture is organized and searched.

| Listener | Host | Port | Traffic |
| --- | --- | ---: | --- |
| `cli.claude-code` | `localhost` | 48723 | Claude Code model API traffic |
| `cli.codex` | `localhost` | 48724 | Codex CLI model API traffic |
| `app.cursor` | `localhost` | 48725 | Cursor desktop traffic |
| `app.claude` | `localhost` | 48726 | Claude desktop traffic |
| `app.codex` | `localhost` | 48727 | Codex desktop traffic |
| `cli.claude-code-proxy` | `localhost` | 48728 | Claude Code proxy traffic |
| `cli.codex-proxy` | `localhost` | 48729 | Codex CLI proxy traffic |
| `cli.codex-backend` | `localhost` | 48730 | Codex CLI backend traffic |
| `app.conductor` | `localhost` | 48731 | Conductor.app desktop traffic |
