# Adapter refactor

This is the current working plan for `internal/adapter/`. It is intentionally
short. Keep old implementation detail out of this file; use it to avoid
repeating completed work and to choose the next slice.

## Goal

`internal/adapter/` exposes an OpenAI Chat Completions compatible surface for
Cursor and an Anthropic-native ingress for Claude Code. The root adapter owns
HTTP decode, auth, model resolution, provider dispatch, and OpenAI response
encoding. Each provider owns request shaping, transport, upstream response
parsing, and provider-specific lifecycle.

## Current Architecture

| Package                       | Role                                                          |
| ----------------------------- | ------------------------------------------------------------- |
| `internal/adapter/`           | HTTP routes, auth, dispatch, server lifecycle                 |
| `internal/adapter/openai/`    | OpenAI wire types and SSE framing                             |
| `internal/adapter/cursor/`    | Cursor request normalization and product tool semantics       |
| `internal/adapter/resolver/`  | Model family, provider, effort, and context-budget resolution |
| `internal/adapter/provider/`  | Provider contract, event writer, shared results and errors    |
| `internal/adapter/anthropic/` | Anthropic OAuth provider and native `/v1/messages` transport  |
| `internal/adapter/codex/`     | Codex websocket provider, session cache, turn metadata        |
| `internal/adapter/render/`    | Normalized event model to OpenAI chunks                       |
| `internal/adapter/runtime/`   | Lifecycle logging and notice surfacing                        |

## OpenAI Success Response Audit

The success response shapes in `internal/adapter/openai/types.go` are
intentional OpenAI-compatible wire types. `ChatResponse`, `StreamChunk`,
`StreamChoice`, `StreamDelta`, `Usage`, and `PromptTokensDetails` are part of
the `/v1/chat/completions` response contract even when Anthropic or Codex owns
the upstream transport.

Intentional response-boundary uses:

- `internal/adapter/` uses facade aliases from `openai_aliases.go` for root
  dispatch, passthrough usage extraction, context tracking, and legacy SSE
  helpers.
- `internal/adapter/provider/` returns `adapteropenai.Usage` and optional
  `adapteropenai.ChatResponse` so provider execution can hand the root adapter
  a final OpenAI-compatible result.
- `internal/adapter/render/` returns `adapteropenai.StreamChunk` values because
  normalized provider events are rendered into client-visible OpenAI SSE
  chunks.
- `internal/adapter/anthropic/backend/` and `internal/adapter/codex/` build
  `adapteropenai.ChatResponse`, `adapteropenai.StreamChunk`, and
  `adapteropenai.Usage` values at the provider-owned translation edge.

Avoidable alias follow-up: `internal/adapter/runtime/wire.go` appears wider
than current use. Repository search showed no external `adapterruntime.*`
references for its OpenAI success aliases, and the runtime package itself only
uses `ChatResponse` unqualified today. Narrowing that file can be a small
follow-up, but do not remove the canonical `openai` success types.

## Completed Memory

See [`adapter-refactor-history.md`](./adapter-refactor-history.md) for the
concise completed-task list. Do not re-open those tasks unless current code
evidence shows a regression.

## Next Slice

1. Continue `CLYDE-157`: finish trace/span propagation across adapter,
   daemon, provider, and capture logs. The repo-wide correlation contract and
   non-adapter audit are in [`../../AGENTS.md`](../../AGENTS.md).

## Global Remaining Set

- `CLYDE-134`: Done. Native ingress reaches real Anthropic OAuth; live bucket
  currently returns 429.
- `CLYDE-150`: In Progress. Keep Anthropic parity and generated wire identity
  in sync with the local XDG baseline.
- `CLYDE-151`: Todo. Validate Codex turn metadata against live ChatGPT Pro
  traces.
- `CLYDE-152`: Done. Websocket reuse and longer-turn stability look good enough
  to close.
- `CLYDE-153`: In Progress. Move remaining tests under provider, render,
  runtime, and root ownership boundaries.
- `CLYDE-154`: In Progress. Sweep dead imports and dead adapter-local types
  after bridge/fallback deletion.
- `CLYDE-155`: Todo. Generate provider wire types from `research/` and remove
  raw payload probing.
- `CLYDE-157`: In Progress. Add trace/span IDs across adapter, daemon,
  provider, and capture logs. Contract and non-adapter audit documented in
  [`../../AGENTS.md`](../../AGENTS.md).
- `CLYDE-158`: In Progress. Fix or explain context-window mismatch behavior for
  Cursor adapter models.
- `CLYDE-159`: Todo. Reproduce Codex long-running tasks stopping too early.
- `CLYDE-160`: Todo. Reproduce Claude long-running tasks stopping too early.
- `CLYDE-161`: Todo. Split logging architecture by concern and evaluate
  per-request log bundles.
- `CLYDE-162`: Done. Background completion failures now carry enough
  correlation to debug the Cursor-side completion path.
- `CLYDE-163`: Done. Cursor auto-summarization remains client-side; Clyde's
  responsibility is correct context reporting and preflight behavior.
- `CLYDE-165`: Done. Daemon-owned always-on MITM with rolling XDG
  baselines and drift logs. MITM is config-driven internal infrastructure, not
  a user-facing `clyde` CLI surface.
- `CLYDE-169`: Done. Adapter model mappings, native Codex aliases, and
  observed Codex context windows are config-driven.

## Non-Negotiables

- Do not reintroduce subprocess or app-server fallback paths.
- Do not add new root-owned Anthropic or Codex request builders.
- Keep OpenAI SSE framing in `openai/` and normalized event handling in
  `render/` plus provider writers.
- Keep provider-specific terminal-state mapping inside provider-owned code.
- Do not add `any`, `interface{}`, `map[string]any`, or `[]any` to touched
  production adapter code.

## Evidence Files

- [`adapter-refactor-research.md`](./adapter-refactor-research.md): compact
  product and protocol facts still useful for implementation.
- [`cursor-error-shape-research.md`](./cursor-error-shape-research.md):
  repeatable Cursor error-shape probe matrix and evidence table.
- [`snapshot-v2-design.md`](./snapshot-v2-design.md): compact Snapshot v2
  status for `CLYDE-150`.
