# AGENTS.md

This file contains durable instructions for coding agents working in this repository.
Keep it short, current, and focused on rules that should affect day-to-day code changes.
Move long runbooks, dated audits, generated examples, and machine-specific workflows into docs.

## Project purpose

Clyde is a Go CLI and daemon for raw provider artifact reading plus adapter, MITM, ingress, logging, MCP, and transcript export.

Core surfaces:

- A first-party `clyde` CLI that owns Clyde-specific conversation, export, logs, daemon, MITM, and MCP commands.
- A raw conversation index that reads Claude and Codex artifacts without writing provider files.
- An MCP server for conversation listing, retrieval, context windows, search, analysis, and export.
- An OpenAI-compatible adapter under `internal/adapter/`.
- A MITM capture proxy under `internal/mitm/`.
- Provider auth behind generic adapter boundaries, with provider-specific credential readers injected by the daemon.

Clyde must not own, create, resume, rename, isolate, compact, wrap, or present provider sessions. Users should run raw `claude` and raw `codex` directly for interactive work.

## Source of truth

Prefer code and tests over this file for exact behavior.

- Use `cmd/clyde/main.go` for current CLI routing.
- Use `internal/conversation/` for raw provider artifact indexing, loading, search, and export boundaries.
- Use `docs/conversations.md` for durable conversation model, compaction segment, and export selector terminology.
- Use `internal/mcpserver/` for MCP tool names and argument contracts.
- Use `internal/config/` for supported config file formats and fields.
- Use `internal/daemon/` for daemon startup, adapter/MITM listener ownership, reload, and background conversation-index refresh.
- Use `internal/adapter/` for adapter, Cursor, Codex, Anthropic, model routing, and request-shape details.
- Use `internal/mitm/` plus `internal/providers/*/mitmcontrib/` for MITM provider registration, route claims, identity facets, and capture extensions.
- Use `docs/logging/` and `internal/slogger/` for logging, sink, inventory, and correlation contracts.
- Use `docs/cursor.md` for the empirical reasons behind Cursor-specific adapter rules.
- Use `docs/reorient/overview.md` for the two-tier reorient-after-compaction delivery and the MITM summary-injection hook.

Do not add stale snapshots of command tables, schemas, request payloads, local machine setup, or dated audits to this file. Add links or brief pointers instead.

## Raw Conversation Rules

- Conversation IDs are stable derived IDs: `claude:<provider-session-id>`, `codex:<thread-id>`, or `artifact:<path-hash>` when the provider ID is missing.
- Clyde reads provider-owned artifacts only. It must not mutate Claude or Codex transcript files, settings, metadata, or runtime state.
- Startup must load the last completed conversation cache quickly and refresh in a debounced background worker.
- Refreshes must be idempotent and keyed by provider, artifact path, size, and mtime.
- Stale cache data is acceptable while a refresh is running.
- CLI and MCP operations must use `conversation_id` terminology. Do not add session-name aliases.

## CLI And MCP

- Running `clyde` with no arguments must show help.
- Unknown arguments must fail through Cobra/help. Do not forward unknown arguments to provider CLIs.
- Each conversation operation is declared once in `internal/clispec` and rendered onto both the terminal command and the MCP tool, with the terminal commands grouped under the `conversation` parent. Add or change an operation there, not in two places.
- The CLI and MCP operation sets stay aligned structurally. `internal/clispec/alignment_test.go` reads the one registry and fails if an operation, parameter, or required flag exists on one surface but not the other, so there is no separate list to maintain here.

## Daemon Reload

Preserve zero-bind-gap daemon reload semantics when changing `internal/daemon/`, `internal/adapter/`, or `internal/mitm/`.

- Reload must re-exec the current daemon binary and inherit daemon-owned listener file descriptors.
- Reload must reject listener address changes that require a full restart.
- A reload child must not initiate another reload until it owns the daemon process lock.
- After child readiness, the old generation must stop accepting public traffic and drain or close existing traffic according to the daemon implementation.
- Existing long-lived HTTP, CONNECT, websocket, MCP, and gRPC streams may stay on the old process until graceful drain completes.

A config edit no longer always re-execs. The watcher classifies the change (`config.ClassifyConfigChange`) and either applies it in process, waits for quiet then reloads, rebinds, or asks for a restart. See `docs/reload-and-hot-apply.md`.

Keep detailed reload behavior in daemon code comments, tests, or dedicated docs rather than expanding this file.

## Tracked Long-Lived Work

Any subsystem that owns long-lived state crossing reload, shutdown, or force-close boundaries MUST register that state with `internal/livetrack`. This includes HTTP connections, MITM CONNECT tunnels, gRPC streams, provider websockets, SSE readers, MCP stdio handlers, file watchers, async workers, and capture-file flocks.

Forbidden in subsystems that have adopted livetrack:

- Bare `http.Server.Shutdown` without a paired registry drain.
- Bare context cancellation as the sole shutdown mechanism for long-lived goroutines that hold OS resources.
- Goroutine fanout without registry registration.
- Per-subsystem hand-rolled equivalents of `WaitForIdle`, `ActiveCount`, or `CloseAll`.

Registration is now enforced by the API, not by convention: `livetrack.New`, `Registry.Drain`, and `Registry.DrainWith` are unexported, so the only way to construct a registry is `livetrack.Attach` (which binds it to the daemon lifecycle group) and the only way to drain is `Group.Quiesce`. See `docs/reload-and-hot-apply.md`.

The motivating empirical case for long-lived Cursor backend keepalives is documented in `docs/cursor.md`.

## Type Hygiene

This repository is pre-alpha. Prefer strict type safety over loose compatibility.

- Do not introduce `any`, `interface{}`, `map[string]any`, `[]any`, or equivalent open-ended payloads in production Go code.
- Do not use empty marker structs such as `struct{}` or empty JSON payloads such as `{}` to represent protocol messages, request params, response params, config sections, or domain state.
- Wire, config, RPC, logging, and domain payloads must use named structs, typed fields, typed slices, typed maps, and explicit enum-like types where applicable.
- Provider identities should use typed enums at generic boundaries.
- If upstream data is a union, model supported variants explicitly and reject or ignore unsupported variants intentionally at the boundary.
- If JSON must remain partially opaque for an external contract, isolate that opacity at the smallest edge with a named type and a comment citing the contract.
- Tests should assert concrete typed shapes and should not build fixtures with loose maps when production code has or should have concrete types.

Existing loose types are technical debt, not precedent. When touching a loose surface, either replace it with enumerated types in the same change or leave a narrow follow-up note if the refactor is larger than the active task.

## Testing And Verification

Write tests alongside behavior changes when practical. Cover success and error paths, keep tests independent, and use descriptive test names.

### Failing make steps

If any step of a `make` target fails, fix the underlying code, test, configuration, or documentation honestly and truthfully.

- Do not turn off, skip, weaken, delete, silence, baseline, or otherwise circumvent a failing `make` step to make the target pass.
- Do not add `|| true`, ignore exit codes, narrow target scopes, raise thresholds, lower coverage expectations, or remove checks unless the user explicitly asks for that exact policy change.
- The fix must be a real code or test correction that addresses the failure while preserving the intent of the check.
- If the failure appears to be caused by an external outage, missing local credential, or unavailable toolchain, report that blocker with the exact command and error instead of bypassing the step.

### Cursor live verification

For changes that affect the OpenAI-compatible adapter, Cursor BYOK ingress, SSE rendering, thinking blocks, tool calls, file reads, or provider request builders, unit tests are necessary but may not be sufficient.

Use the real Cursor client for final verification when the rendered chat output or actual SSE bytes matter. Keep prompts read-only and include a unique probe id. Build, install, and reload the daemon before the probe. Operator-specific automation belongs in a separate runbook, not in this file.

Use `docs/testing/overview.md` for the live daemon test harness and its parallel-run port overrides.

## Adapter And Model Routing

The adapter is a safety boundary. For model aliases, effort tiers, context budgets, request shaping, and provider-specific behavior, prefer config-driven and typed resolver paths over hard-coded facts.

### Adapter surfaces

The adapter HTTP listener serves three route families. They share the listener, the auth pipeline, and the error boundary, but each carries its own envelope shape and its own production status. Do not conflate route families.

| Route family | Paths | Status |
| --- | --- | --- |
| OpenAI-compatible | `/v1/chat/completions`, `/v1/models`, `/v1/completions` | Production. Cursor BYOK and any OpenAI-SDK-compatible client. |
| Native Anthropic | `/v1/messages`, `/v1/messages/count_tokens` | Code shipped, untested in production. Proposed for cross-provider use. |
| Health | `/healthz`, `/` | Ops only. |

The MITM proxy is a separate surface, not an adapter route. It runs on its own per-client listeners under `internal/mitm/`, one loopback port per traffic type, acts as a forward proxy for arbitrary HTTPS targets, and is not subject to the adapter error boundary.

### Routing rules

- Do not add new hard-coded model facts unless the task explicitly requires it and the follow-up toward config-driven behavior is documented.
- Preserve adapter-side preflight for known context-window overflows.
- Do not log raw prompts, request bodies, response bodies, tokens, credentials, cookies, API keys, or personal data unless an explicit local debugging policy enables sanitized or raw body logging.
- Reasoning round-trip is per-provider, configured under `[adapter.<provider>.reasoning]`.
- Cross-provider thinking replay must inject foreign provider reasoning as plain text instead of recreating native signed or encrypted thinking blocks.

## Error Boundary

Every adapter HTTP response with a non-2xx status MUST go through the adapter error boundary so the calling client receives a parsable, route-correct envelope with the chosen message preserved in `error.message`.

- Handlers return a typed adapter error from the generic adapter.
- Route-family renderers live in provider packages and own their envelope shape.
- Pre-headers errors and mid-stream errors both go through the boundary's typed entry points.
- Upstream failures classify into a typed upstream-code class and flow through the route-family-specific upstream-error mapper.

OpenAI-compatible route family rule:

- Every non-2xx upstream MUST be returned as HTTP 400 + `invalid_request_error` + a typed `upstream_*` code.
- Do not preserve upstream 5xx or 429 status codes on the response.
- Do not map upstream rate-limits to OpenAI `rate_limit_error`.

The empirical Cursor reason for this rule is in `docs/cursor.md`.

## Logging And Observability

Use structured `log/slog` logging for production diagnostics. Prefer context-aware logging when a `context.Context` exists.

- Log meaningful lifecycle boundaries, external calls, state mutations, retries, fallbacks, completions, and failures.
- Include fields that make events queryable, such as `component`, `subcomponent`, `request_id`, `trace_id`, `span_id`, `parent_span_id`, `conversation_id`, `model`, `duration_ms`, `attempt`, `count`, `path`, `status`, and `err`.
- Use explicit concern loggers from `internal/slogger` at subsystem boundaries when possible.
- Propagate the correlation context through HTTP, gRPC, daemon jobs, provider requests, MCP handlers, and CLI command contexts.
- Do not invent unrelated trace ids in lower-level helpers when a caller context exists.
- Keep hot-path detail at `Debug`, and keep healthy steady-state requests to a small number of `Info` events.
- Do not use `fmt.Print`, `fmt.Println`, `fmt.Printf`, or standard-library `log.Print` for operational logging. `fmt.Fprint*` is acceptable for intentional user-facing command output.

Default log paths are under `$XDG_STATE_HOME/clyde`; when `XDG_STATE_HOME` is unset, use `~/.local/state/clyde`.

- Main daemon log: `clyde-daemon.jsonl`.
- Main CLI log: `clyde-cli.jsonl`.
- Concern logs: `logs/<concern-path>.jsonl`.
- Dedicated Codex sidecar log: `codex.jsonl`, unless `CLYDE_CODEX_LOG_PATH` overrides it.
- MITM capture store: `mitm/capture.db` (SQLite; full decoded request/response bodies plus per-request metadata, one row per exchange with a side table for bodies; each row is tagged by `client` (the listener id, or `adapter.<provider>` for in-code BYOK egress) and an autoclassified `concern`).
- MITM wire legs: `logs/providers/mitm/wire.jsonl` via the `providers.mitm.wire` concern; join to capture.db rows on `request_id`/`trace_id`.
- macOS LaunchAgent stderr/stdout fallback: `daemon.log`.

Useful concern roots include `adapter.http`, `adapter.chat`, `adapter.models`, `adapter.providers`, `providers.claude`, `providers.codex`, `providers.mitm`, `daemon.rpc`, `daemon.workers`, `process.daemon`, and `mcp.server`.

## Networking And Security

For local adapter, MITM, test server, and example upstream addresses, use `localhost` wherever the consumer accepts hostnames. When a literal bind or URL host is required, use IPv6 loopback `[::1]`.

Do not introduce `127.0.0.1`, `0.0.0.0`, wildcard binds, LAN addresses, or public listener defaults unless the user explicitly asks for an externally reachable service and the security implications are handled in the same change.

Store keys and tokens in environment variables or file references only. Reference sensitive data by variable name or file path in output, logs, tests, and docs.

## Documentation Hygiene

**No agent creates, rewrites, restructures, or regenerates documentation unless the user explicitly directs that specific change.** This covers everything under `docs/`, this file, and any other Markdown doc. Docs here are durable and human-owned, so an unrequested rewrite is a regression even when it reads as an improvement. Correcting a typo or a broken link in a file you are already editing for a code change is fine; reshaping, condensing, expanding, or regenerating a doc is not, without an explicit instruction.

Keep `AGENTS.md` durable and concise.

- Prefer small files with one clear responsibility.
- Keep one entity or logical responsibility per file when practical.
- Name files for the responsibility they own.
- Prefer full domain names in identifiers over abbreviations.
- Wrap errors with operation context and the relevant identifier or backend.
- Add package doc comments, exported type comments, and comments for non-obvious exported fields.
- Do not add long JSON examples, full shell scripts, generated schemas, dated audit tables, local workstation facts, or historical incident notes.
- Move runbooks to `docs/`, and point to them from this file only when agents need to know they exist.
- Move task lists and stale audit findings to issues or dedicated planning docs.
- When behavior changes, update the code, tests, and closest specific documentation. Update this file only when the durable agent rule changes.
