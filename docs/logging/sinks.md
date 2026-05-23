# Logging Sinks

A sink is a stable destination class, not a concrete path. `logevent.DefaultSinksForEvent` assigns sink names to request events from event identity, surface, and provider facets.

Core sinks:

- `process` for the main daemon or CLI process log.
- `concern` for the concern-specific JSONL tree.
- `inventory_index` for log discovery metadata.
- `per_chat` when a request has a chat key.
- `mitm_capture_index` for MITM capture index events.
- `mitm_raw` when MITM raw sidecar paths are present.
- `provider_sidecar` when a Codex or Anthropic provider facet is present.

Sink routing should live in the central sink model. Adapter, MITM, provider, and transcript code should not invent separate user-facing sink names for request traffic.

Use `clyde logs inventory` to resolve sink classes to current files under the active state root. Do not hard-code local paths into diagnostics or docs when inventory can discover them.
