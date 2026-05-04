# [AGENTS.md](http://AGENTS.md)

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
- Use `internal/adapter/` and `docs/adapter-refactor/` for adapter, Cursor, Codex, Anthropic, model routing, and request-shape details.
- Use `docs/SLOG.md` and `internal/slogger/` for detailed logging and correlation contracts when those files exist and are current.

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

Preserve zero-bind-gap daemon reload semantics when changing `internal/daemon/run.go`, `internal/adapter/`, or `internal/webapp/`.

- Reload must re-exec the current daemon binary and inherit daemon-owned listener file descriptors.
- Reload must reject listener address changes that require a full restart.
- A reload child must not initiate another reload until it owns `daemon.process.lock`.
- After child readiness, the old generation must stop accepting public traffic and drain or close existing traffic according to the daemon implementation.
- Existing gRPC streams may stay on the old process until graceful drain completes.
- Active session runtime dirs must survive reload drain so wrappers and remote-control sockets can reacquire against the child.

Keep detailed reload behavior in daemon code comments, tests, or dedicated docs rather than expanding this file.

## Type hygiene

This repository is pre-alpha. Prefer strict type safety over loose compatibility.

- Do not introduce `any`, `interface{}`, `map[string]any`, `[]any`, or equivalent open-ended payloads in production Go code.
- Do not use empty marker structs such as `struct{}` or empty JSON payloads such as `{}` to represent protocol messages, request params, response params, config sections, or domain state.
- Wire, config, RPC, logging, and domain payloads must use named structs, typed fields, typed slices, typed maps, and explicit enum-like string types where applicable.
- If upstream data is a union, model supported variants explicitly and reject or ignore unsupported variants intentionally at the boundary.
- If JSON must remain partially opaque for an external contract, isolate that opacity at the smallest edge with a named type and a comment citing the contract.
- For Codex protocol surfaces, prefer researched or generated source-of-truth schemas under `research/codex/` when available.
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

Use the real Cursor client for final verification when the rendered chat output or actual SSE bytes matter. Keep prompts read-only and include a unique probe id. Build, install, and reload the daemon before the probe. Keep machine-specific Hammerspoon scripts and screen names in a separate runbook, not in this file.

## Adapter and model routing

The adapter is a safety boundary. For model aliases, effort tiers, context budgets, request shaping, and provider-specific behavior, prefer config-driven and typed resolver paths over hard-coded facts.

- Do not add new hard-coded model facts unless the task explicitly requires it and the follow-up toward config-driven behavior is documented.
- Keep Cursor/Codex observations in `docs/adapter-refactor/` or tests, not in this file.
- Preserve adapter-side preflight for known context-window overflows. Do not open an upstream provider turn when Clyde can already tell the request exceeds the resolved model budget.
- Do not log raw prompts, request bodies, response bodies, tokens, credentials, cookies, API keys, or personal data unless an explicit local debugging policy enables sanitized or raw body logging.

## Logging and observability

Use structured `log/slog` logging for production diagnostics. Prefer context-aware logging when a `context.Context` exists.

- Log meaningful lifecycle boundaries, external calls, state mutations, retries, fallbacks, completions, and failures.
- Include fields that make events queryable, such as `component`, `subcomponent`, `request_id`, `trace_id`, `span_id`, `parent_span_id`, `session`, `session_id`, `model`, `duration_ms`, `attempt`, `count`, `path`, `status`, and `err`.
- Use explicit concern loggers from `internal/slogger` at subsystem boundaries when possible.
- Propagate `internal/correlation.Context` through HTTP, gRPC, daemon jobs, provider requests, MCP handlers, and CLI command contexts.
- Do not invent unrelated trace ids in lower-level helpers when a caller context exists. Thread the context down instead.
- Keep hot-path detail at `Debug`, and keep healthy steady-state requests to a small number of `Info` events.
- Do not use `fmt.Print`, `fmt.Println`, `fmt.Printf`, or standard-library `log.Print` for operational logging. `fmt.Fprint*` is acceptable for intentional user-facing command output.

Put detailed logging setup examples, correlation audits, and backlog tables in logging docs or issue trackers rather than this file.

## Debugging and logs

Start debugging by checking Clyde's structured logs before guessing from symptoms. Default log paths are under `$XDG_STATE_HOME/clyde`; when `XDG_STATE_HOME` is unset, use `~/.local/state/clyde`.

- Main daemon log: `$XDG_STATE_HOME/clyde/clyde-daemon.jsonl`.
- Main TUI log: `$XDG_STATE_HOME/clyde/clyde-tui.jsonl`.
- Concern logs: `$XDG_STATE_HOME/clyde/logs/<concern-path>.jsonl`, where concern names from `internal/slogger/concerns.go` map dots to nested paths.
- Dedicated Codex sidecar log: `$XDG_STATE_HOME/clyde/codex.jsonl`, unless `CLYDE_CODEX_LOG_PATH` overrides it.
- MITM captures: `$XDG_STATE_HOME/clyde/mitm/capture.jsonl` by default, or the configured `[mitm].capture_dir`.
- macOS LaunchAgent stderr/stdout fallback: `~/.local/state/clyde/daemon.log`.

Operators may override main process log paths with `[logging.paths].daemon`, `[logging.paths].tui`, or `CLYDE_SLOG_PATH`. Check the active config before assuming the defaults.

For adapter, Cursor, Codex, Anthropic, passthrough, MITM, live-session, and daemon issues, prefer the matching concern log first. Useful concern roots include `adapter.http`, `adapter.chat`, `adapter.providers`, `providers.claude`, `providers.codex`, `providers.mitm`, `daemon.rpc`, `daemon.workers`, `session`, `process.daemon`, and `ui`.

Use correlation fields to follow one operation across files: `trace_id`, `span_id`, `parent_span_id`, `request_id`, `cursor_request_id`, `cursor_conversation_id`, `upstream_request_id`, and `upstream_response_id`. Avoid raw body logging unless the user explicitly enables a safe local debugging policy, and never paste secrets, prompts, tokens, cookies, or API keys into chat.

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

