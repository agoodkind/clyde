# Logging Sinks

A sink is a stable destination class, not a concrete path. `logevent.DefaultSinksForEvent` assigns sink names to request events from event identity, surface, and provider facets.

Core sinks, the `logevent.SinkName` routing taxonomy in `internal/logevent/logevent.go`:

- `process` for the main process log.
- `concern` for the concern JSONL tree. MITM wire legs route here through the `providers.mitm.wire` concern at `logs/providers/mitm/wire.jsonl`.
- `per_request` for per-request logs.
- `per_chat` when the event has a chat key.
- `provider_sidecar` when `SinkHints.NeedsProviderSidecar` is set by a Codex or Anthropic facet.
- `inventory_index` for log discovery metadata.

`DefaultSinksForEvent` always emits `process`, `concern`, and `inventory_index`. It adds `per_chat` when `Identity.ChatKey` is non-empty, and it adds `provider_sidecar` from the aggregated typed `SinkHints` contributed by facets rather than by branching on surface or provider.

MITM request/response bodies are not a sink; they persist to the SQLite capture store at `mitm/capture.db`. MITM wire legs route through the `concern` sink to `logs/providers/mitm/wire.jsonl`.

These sink names live in two namespaces: the `logevent.SinkName` routing taxonomy above, and the config-facing declarative roster (`config.LoggingSinkSpec` and `IsKnownLoggingSink` in `internal/config/logging_config.go`) that operators configure with `[logging.sinks.<name>]` blocks, where `inventory_index` reuses the config constant as its wire value so the two namespaces share one source for that name.

Sink routing should live in the central sink model. Adapter, MITM, provider, and transcript code should not invent separate user-facing sink names for request traffic.

Use `clyde logs inventory` to resolve sink classes to current files under the active state root. Do not hard-code local paths into diagnostics or docs when inventory can discover them.
