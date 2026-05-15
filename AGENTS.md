# AGENTS.md

This file contains durable instructions for coding agents working in this repository.
Keep it short, current, and focused on rules that should affect day-to-day code changes.
Move long runbooks, dated audits, generated examples, and machine-specific workflows into docs.

## Project purpose

Clyde is a Go wrapper and coordination layer for LLM tools, with first-class support for Claude Code and Codex today. It has these core surfaces:

- A first-party `clyde` CLI that owns Clyde-specific commands and forwards provider-native work to the appropriate underlying tool.
- A TUI dashboard for managing existing sessions.
- A long-lived daemon for the adapter, OAuth, MCP, pruning, and live-session coordination.
- Append-only compaction.
- An OpenAI-compatible adapter under `internal/adapter/`.
- An MCP server for session search, listing, and context.

Treat Clyde as a thin, non-invasive wrapper around supported LLM tools. Do not patch provider binaries or reimplement provider-native behavior when forwarding or wrapping is enough.

## Source of truth

Prefer code and tests over this file for exact behavior.

- Use `cmd/clyde/main.go`, `cmd/root.go`, and `cmd/dispatch.go` for current CLI routing.
- Use `internal/session/`, `internal/providers/claude/`, and related tests for session metadata, hooks, transcript paths, and delete behavior.
- Use `internal/config/` for supported config file formats and fields.
- Use `internal/daemon/` for daemon reload, listener handoff, and live-session ownership.
- Use `internal/adapter/` for adapter, Cursor, Codex, Anthropic, model routing, and request-shape details.
- Use `docs/SLOG.md` and `internal/slogger/` for detailed logging and correlation contracts.
- Use `docs/cursor.md` for the empirical reasons behind Cursor-specific rules.

Do not add stale snapshots of command tables, schemas, request payloads, local machine setup, or dated audits to this file. Add links or brief pointers instead.

## Architecture rules

### TUI as a renderer

Treat the TUI as a dumb renderer over daemon-owned and domain-owned state.

- Put business logic, normalization, filtering, aggregation, transcript shaping, provider accounting, and session state derivation upstream in shared packages or daemon RPC construction.
- Do not add TUI-only semantic cleanup when the same logic belongs in `internal/transcript`, `internal/providers/claude/`, `internal/adapter/`, or daemon/server code.
- Non-UI work triggered from the TUI must run through daemon-owned or daemon-backed async paths.
- TUI draw paths and event handlers must not block on transcript parsing, filesystem scans, config probing, RPC fan-out, context probes, export aggregation, or similar non-render work.
- The TUI may own layout, focus, scrolling, wrapping, truncation, visual grouping, badges, and status text.

### Daemon-owned live sessions

Live interactive sessions are daemon-owned. TUI, webapp, and command surfaces must use provider-neutral live-session RPCs instead of probing provider files, sockets, bridge state, transcript tails, or send primitives directly.

Provider-specific harnesses, including Claude bridge behavior, Claude pty injection, transcript tailing, Codex tmux, or browser automation, belong behind the daemon live-session backend.

### Daemon reload

Preserve zero-bind-gap daemon reload semantics when changing `internal/daemon/`, `internal/adapter/`, or `internal/webapp/`.

- Reload must re-exec the current daemon binary and inherit daemon-owned listener file descriptors.
- Reload must reject listener address changes that require a full restart.
- A reload child must not initiate another reload until it owns the daemon process lock.
- After child readiness, the old generation must stop accepting public traffic and drain or close existing traffic according to the daemon implementation.
- Existing gRPC streams may stay on the old process until graceful drain completes.
- Active session runtime dirs must survive reload drain so wrappers and remote-control sockets can reacquire against the child.

Keep detailed reload behavior in daemon code comments, tests, or dedicated docs rather than expanding this file.

### Tracked sessions

Any subsystem that owns long-lived state crossing reload, shutdown, or force-close boundaries MUST register that state with `internal/livetrack`. This includes HTTP connections, MITM CONNECT tunnels, gRPC streams, provider websockets, SSE readers, MCP stdio handlers, browser tabs, file watchers, async workers, and capture-file flocks.

Forbidden in subsystems that have adopted livetrack:

- Bare `http.Server.Shutdown` without a paired registry drain.
- Bare context cancellation as the sole shutdown mechanism for long-lived goroutines that hold OS resources (sockets, fds, flocks).
- Goroutine fanout without registry registration; the registry IS the inventory the daemon reload chain queries.
- Per-subsystem hand-rolled equivalents of `WaitForIdle`, `ActiveCount`, `CloseAll`. The HTTP-conn-state map pattern that livetrack replaced must not be reintroduced.

The motivating empirical case (Cloudflare keepalive on Cursor backends holding tunnels open indefinitely) is documented in `docs/cursor.md`.

## Type hygiene

This repository is pre-alpha. Prefer strict type safety over loose compatibility.

- Do not introduce `any`, `interface{}`, `map[string]any`, `[]any`, or equivalent open-ended payloads in production Go code.
- Do not use empty marker structs such as `struct{}` or empty JSON payloads such as `{}` to represent protocol messages, request params, response params, config sections, or domain state.
- Wire, config, RPC, logging, and domain payloads must use named structs, typed fields, typed slices, typed maps, and explicit enum-like string types where applicable.
- If upstream data is a union, model supported variants explicitly and reject or ignore unsupported variants intentionally at the boundary.
- If JSON must remain partially opaque for an external contract, isolate that opacity at the smallest edge with a named type and a comment citing the contract.
- Tests should assert concrete typed shapes and should not build fixtures with loose maps when production code has or should have concrete types.

Existing loose types are technical debt, not precedent. When touching a loose surface, either replace it with enumerated types in the same change or leave a narrow follow-up note if the refactor is larger than the active task.

## Testing and verification

Write tests alongside behavior changes when practical. Cover success and error paths, keep tests independent, and use descriptive test names.

Common checks include:

- `make test`
- `make lint`
- `make fmt` followed by `git diff --exit-code`
- `make staticcheck`
- `make staticcheck-extra`
- `make deadcode`
- `make audit`
- `make govulncheck`
- `make build`

### Failing make steps

If any step of a `make` target fails, fix the underlying code, test, configuration, or documentation honestly and truthfully.

- Do not turn off, skip, weaken, delete, silence, baseline, or otherwise circumvent a failing `make` step to make the target pass.
- Do not add `|| true`, ignore exit codes, narrow target scopes, raise thresholds, lower coverage expectations, or remove checks unless the user explicitly asks for that exact policy change.
- The fix must be a real code or test correction that addresses the failure while preserving the intent of the check.
- If the failure appears to be caused by an external outage, missing local credential, or unavailable toolchain, report that blocker with the exact command and error instead of bypassing the step.

### Cursor live verification

For changes that affect the OpenAI-compatible adapter, Cursor BYOK ingress, SSE rendering, thinking blocks, tool calls, file reads, or provider request builders, unit tests are necessary but may not be sufficient.

Use the real Cursor client for final verification when the rendered chat output or actual SSE bytes matter. Keep prompts read-only and include a unique probe id. Build, install, and reload the daemon before the probe. Operator-specific automation belongs in a separate runbook, not in this file.

## Adapter and model routing

The adapter is a safety boundary. For model aliases, effort tiers, context budgets, request shaping, and provider-specific behavior, prefer config-driven and typed resolver paths over hard-coded facts.

### Adapter surfaces

The adapter HTTP listener serves three route families. They share the listener, the auth pipeline, and the error boundary, but each carries its own envelope shape and its own production status. **STRICT RULE: do not conflate route families. Rules, fixes, and rationale on one route family do not transfer to another.**

| Route family | Paths | Status |
|---|---|---|
| OpenAI-compatible | `/v1/chat/completions`, `/v1/models`, `/v1/completions` | Production. Cursor BYOK and any OpenAI-SDK-compatible client. |
| Native Anthropic | `/v1/messages`, `/v1/messages/count_tokens` | Code shipped, untested in production. Proposed for cross-provider use. |
| Health | `/healthz`, `/` | Ops only. |

The MITM proxy is a **separate surface, not an adapter route**. It runs on its own listener under `internal/mitm/`, acts as a forward proxy for arbitrary HTTPS targets, and is not subject to the adapter error boundary. Forward-proxy traffic that traverses MITM (e.g. claude CLI talking to `api.anthropic.com` through the MITM port) is governed by MITM rules, not adapter route-family rules. Do not conflate the two.

### Routing rules (apply across all adapter route families)

- Do not add new hard-coded model facts unless the task explicitly requires it and the follow-up toward config-driven behavior is documented.
- Preserve adapter-side preflight for known context-window overflows. Do not open an upstream provider turn when Clyde can already tell the request exceeds the resolved model budget.
- Do not log raw prompts, request bodies, response bodies, tokens, credentials, cookies, API keys, or personal data unless an explicit local debugging policy enables sanitized or raw body logging.
- Reasoning round-trip is per-provider, configured under `[adapter.<provider>.reasoning]`. Provider-specific levers and defaults live in code and tests.
- Cross-provider thinking replay: synthetic-thinking markers carry `data-origin` on the open marker (`"anthropic"`, `"codex"`, or absent which resolves to unknown for pre-upgrade transcripts). When a request reaches a provider whose family does not match the origin, the receiving adapter must inject the body as plain text rather than reproduce a native thinking content block. The Anthropic mapper at `internal/adapter/anthropic/backend/mapper_impl.go` emits a `TextBlock` in place of `ThinkingBlock`; the Codex sanitizer at `internal/adapter/codex/protocol.go` folds the body into the assistant message text and refuses to emit a Codex `reasoning` input item in `internal/adapter/codex/request_builder.go`. The reason this rule exists: an Anthropic `thinking` block requires a per-block signature no other provider produces, and a Codex `reasoning` input item requires a Codex-issued `rs_*` id with matching `encrypted_content`. Replaying foreign content as native fails upstream validation; injecting as plain text preserves the prior reasoning in context without crossing the signature or encryption boundary. The same rule applies to legacy markers with no origin tag.

### MITM listener and CA

MITM listener and CA are config-driven under `[mitm.listen]` and `[mitm.ca]`. Clyde binds the listener in-process at the configured host and port; daemon reload inherits the listener file descriptor the same way it inherits the daemon, adapter, and webapp listeners. Reload validation rejects address changes. The CA certificate is generated on first start and persisted at the configured cert and key paths; subsequent starts load it from disk. Clyde does not launch GUI clients or wrappers; Cursor's MITM integration is owned by Cursor settings or by a user-owned wrapper outside this repository.

## Layer separation

Clyde is built as a stack of layers, and each layer must stay on its own side of the boundary.

- Each layer declares what it needs through an interface or a primitive contract.
- The next layer down provides what the layer above declared, and nothing more.
- No layer reaches into another layer's semantics, internal types, or presentation choices.
- Provider-specific shape knowledge, including envelopes, headers, wire types, and vendor UX quirks, lives in the provider package. It does not leak into the generic adapter package.
- The generic adapter never imports a provider's envelope type, never constructs a provider envelope literal, and never describes a provider-specific UX behavior in its comments.
- New cross-cutting concerns follow this pattern. Define a contract in a small package with no upstream dependencies. Implementations register themselves at startup. The boundary dispatches by family or by registered key, not by hard-coded provider name.

The error boundary below is the canonical worked example.

## Error boundary

Every adapter HTTP response with a non-2xx status MUST go through the adapter error boundary so the calling client receives a parsable, route-correct envelope with the message we chose preserved in `error.message`. The boundary applies strict dependency inversion: the generic adapter declares interfaces, and each provider package implements them. The boundary never imports a provider envelope type and never constructs a provider envelope literal.

The boundary applies to **adapter HTTP responses only**. The MITM proxy is a separate surface and is governed by MITM rules, not by this boundary; do not extrapolate boundary rules onto MITM forward-proxy traffic.

- Handlers return a typed adapter error from the generic adapter. The boundary picks the route family from the request path and looks up the registered error renderer for that route family. Renderers live in provider packages and own their route family's envelope shape entirely.
- Pre-headers errors and mid-stream errors both go through the boundary's typed entry points. The handoff to the renderer speaks only primitives (type, code, message, param).
- Upstream failures classify into a typed upstream-code class and flow through the route-family-specific upstream-error mapper.

Each route family has its own rule. **STRICT RULE: route family rules are scoped to that route family. Do not share rules, fixes, or rationale across route families, and do not extrapolate to MITM.**

### OpenAI-compatible route family rule

Production rule. Applies to `/v1/chat/completions`, `/v1/models`, and `/v1/completions`.

- Every non-2xx upstream MUST be returned as HTTP 400 + `invalid_request_error` + a typed `upstream_*` code.
- Do not preserve upstream 5xx or 429 status codes on the response.
- Do not map upstream rate-limits to OpenAI `rate_limit_error`.
- The empirical reason this rule exists (Cursor BYOK swaps in generic vendor fallback chrome on 5xx/429 and erases the chosen `error.message`) is in `docs/cursor.md`. The rule is enforced regardless of which OpenAI-SDK-compatible client connects.

### Native Anthropic route family rule

This route family exists in code (`/v1/messages`, `/v1/messages/count_tokens`) but is currently unused and untested in production. The proposed use case is cross-provider plumbing.

- Treat as separate from the OpenAI-compatible route family. Do not share rules, fixes, or rationale across the two.
- The renderer should preserve the spec-correct Anthropic error envelope so an Anthropic-SDK client could parse it.
- The rule is provisional until live verification on the route happens. Any production use of this route family requires its own end-to-end tests; do not assume the OpenAI-compatible route family's empirical work covers it.

### Health route family

Health checks (`/healthz`, `/`). No error renderer is registered. Any non-2xx through these paths falls through to the boundary's catch-all.

### Boundary prohibitions and catch-all

- Prohibited in adapter handlers and providers: `http.Error`, `writeJSON` with a non-2xx status, `json.Encode` of an error directly into the response, calling stream-error helpers without going through the boundary's typed entry points, or invoking the internal erroring helper from outside a registered provider renderer. An AST self-test in the adapter package enforces these prohibitions through `make test`.
- The catch-all default for any error reaching the boundary with no registered route renderer is HTTP 400 + `invalid_request_error` + `upstream_failed` with all known fields (provider, upstream status, upstream code, upstream text) folded into `error.message`. The catch-all is the safety net; specific cases layer on top only when an empirical client-side probe shows a different shape is required.

## Logging and observability

Use structured `log/slog` logging for production diagnostics. Prefer context-aware logging when a `context.Context` exists.

- Log meaningful lifecycle boundaries, external calls, state mutations, retries, fallbacks, completions, and failures.
- Include fields that make events queryable, such as `component`, `subcomponent`, `request_id`, `trace_id`, `span_id`, `parent_span_id`, `session`, `session_id`, `model`, `duration_ms`, `attempt`, `count`, `path`, `status`, and `err`.
- Use explicit concern loggers from `internal/slogger` at subsystem boundaries when possible.
- Propagate the correlation context through HTTP, gRPC, daemon jobs, provider requests, MCP handlers, and CLI command contexts.
- Do not invent unrelated trace ids in lower-level helpers when a caller context exists. Thread the context down instead.
- Keep hot-path detail at `Debug`, and keep healthy steady-state requests to a small number of `Info` events.
- Do not use `fmt.Print`, `fmt.Println`, `fmt.Printf`, or standard-library `log.Print` for operational logging. `fmt.Fprint*` is acceptable for intentional user-facing command output.

Wire capture is per-provider, configured under `[adapter.<provider>.wire_capture]`. Provider-specific modes and the shared rotation budget live in code and config.

## Debugging and logs

Start debugging by checking Clyde's structured logs before guessing from symptoms. Default log paths are under `$XDG_STATE_HOME/clyde`; when `XDG_STATE_HOME` is unset, use `~/.local/state/clyde`.

- Cursor BYOK setup against the daemon MITM requires `http.proxy` and related settings in Cursor's user `settings.json`. See `docs/cursor-mitm-setup.md`.
- Main daemon log: `clyde-daemon.jsonl` under the state dir.
- Main TUI log: `clyde-tui.jsonl` under the state dir.
- Concern logs: `logs/<concern-path>.jsonl` under the state dir, where concern names from `internal/slogger/` map dots to nested paths.
- Dedicated Codex sidecar log: `codex.jsonl` under the state dir, unless `CLYDE_CODEX_LOG_PATH` overrides it.
- MITM captures: Codex CLI, Claude CLI, and Cursor traffic. The always-on baseline writes to `mitm/always-on/` under the state dir, with raw TLS-decrypted request and response bytes under `raw/<host>/` and a per-event index in `capture.jsonl`. Captured hosts include `api2.cursor.sh`, `api3.cursor.sh`, and other hosts under `*.cursor.sh` and `*.cursor.com`, as well as `chatgpt.com`, `openai.com`, and `api.anthropic.com`.
- macOS LaunchAgent stderr/stdout fallback: `daemon.log` under `~/.local/state/clyde/`.

Operators may override main process log paths via the logging config block or the `CLYDE_SLOG_PATH` env var. Check the active config before assuming the defaults.

For adapter, Cursor, Codex, Anthropic, passthrough, MITM, live-session, and daemon issues, prefer the matching concern log first. Useful concern roots include `adapter.http`, `adapter.chat`, `adapter.models`, `adapter.providers`, `providers.claude`, `providers.codex`, `providers.mitm`, `daemon.rpc`, `daemon.workers`, `session.lifecycle`, `session.discovery`, `session.domain`, `process.daemon`, `compact.apply`, `compact.preview`, `mcp.server`, `ui.tui`, and `ui.sidecar`. Adapter logs are the primary debugging surface for Cursor ingress. MITM captures provide an independent record of the same exchange; when you suspect adapter pre-processing is at fault, compare the raw TLS-decrypted bytes from MITM against the adapter logs to isolate where behavior diverges.

Use correlation fields to follow one operation across files: `trace_id`, `span_id`, `parent_span_id`, `request_id`, `cursor_request_id`, `cursor_conversation_id`, `cursor_generation_id`, `upstream_request_id`, and `upstream_response_id`. Avoid raw body logging unless the user explicitly enables a safe local debugging policy, and never paste secrets, prompts, tokens, cookies, or API keys into chat.

## Networking and security

For local adapter, webapp, MITM, test server, and example upstream addresses, use `localhost` wherever the consumer accepts hostnames. When a literal bind or URL host is required, use IPv6 loopback `[::1]`.

Do not introduce `127.0.0.1`, `0.0.0.0`, wildcard binds, LAN addresses, or public listener defaults unless the user explicitly asks for an externally reachable service and the security implications are handled in the same change.

Store keys and tokens in environment variables or file references only. Reference sensitive data by variable name or file path in output, logs, tests, and docs.

## Documentation hygiene

Keep `AGENTS.md` durable and concise.

- Prefer small files with one clear responsibility. When a file grows past roughly 200 lines and carries multiple concerns, split it by responsibility rather than by arbitrary code movement.
- Keep one entity or logical responsibility per file when practical, and separate bulk operations from single-item CRUD flows when both exist.
- Name files for the responsibility they own. Avoid `utils.go` and `helpers.go` unless the code is genuinely shared and no narrower name fits.
- Prefer full domain names in identifiers over abbreviations.
- Wrap errors with operation context and the relevant identifier or backend.
- Add package doc comments, exported type comments, and comments for non-obvious exported fields.
- Do not add long JSON examples, full shell scripts, generated schemas, dated audit tables, local workstation facts, or historical incident notes.
- Move runbooks to `docs/`, and point to them from this file only when agents need to know they exist.
- Move task lists and stale audit findings to issues or dedicated planning docs.
- When behavior changes, update the code, tests, and closest specific documentation. Update this file only when the durable agent rule changes.
