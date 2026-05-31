# MITM listeners

Clyde runs one MITM capture listener per distinct traffic type, each on its own
loopback port. A `localhost` listener binds both `[::1]` and `127.0.0.1`, and one
proxy serves both sockets. Every captured exchange in `mitm/capture.db` is tagged
in the `client` column with the group-qualified listener id, so a row names
exactly which client and which kind of traffic produced it.

Listeners are declared under two keyed-table groups: `[mitm.cli.<name>]` for
command-line clients and `[mitm.app.<name>]` for desktop Electron shells. The
table key becomes the listener id `cli.<name>` or `app.<name>`, so the same leaf
name can appear in both groups.

| id | port | traffic |
| --- | --- | --- |
| cli.claude-code | 48723 | Claude Code CLI model API (`ANTHROPIC_BASE_URL`); wire-baseline source |
| cli.claude-code-proxy | 48728 | Claude Code CLI everything else (`HTTPS_PROXY`) |
| cli.codex | 48724 | Codex CLI model API (`model_providers.<id>.base_url`); wire-baseline source |
| cli.codex-backend | 48730 | Codex CLI auth, cloud, analytics (`chatgpt_base_url`) |
| cli.codex-proxy | 48729 | Codex CLI everything else (`HTTPS_PROXY`) |
| app.cursor | 48725 | Cursor desktop Electron shell |
| app.claude | 48726 | Claude desktop Electron shell |
| app.codex | 48727 | Codex desktop Electron shell |

The adapter's outbound BYOK calls to real providers are recorded in code rather
than through a listener. They land in `mitm/capture.db` tagged
`client=adapter.anthropic` or `client=adapter.codex` with full request and
response bodies, and they reach the provider directly without transiting the
proxy, so they never feed the wire baseline.

`clyde mitm status` lists every listener with its bound sockets and reports
whether each socket is up.

## Adapter ingress ports

The adapter HTTP listener is a separate surface from the MITM listeners above.
It can bind two ports so the port a request arrives on records which client
class it is, for accounting only; the split changes nothing about translation,
routing, or error mapping.

| id | port | traffic |
| --- | --- | --- |
| ingress.openai | 11434 | generic OpenAI-compatible clients (`[adapter].port`); tagged `ingress=openai` |
| ingress.cursor | 11435 | Cursor BYOK (`[adapter].cursor_ingress_port`); tagged `ingress=cursor` |

The Cursor port binds only when `[adapter].cursor_ingress_port` is set; with it
unset the adapter binds the primary port alone and every request is tagged
`ingress=openai`. The `ingress` label is recorded on the
`adapter.request.started` and `adapter.request.stream_opened` events.
