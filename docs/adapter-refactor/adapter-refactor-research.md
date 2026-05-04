# Adapter refactor research notes

This file keeps only product and protocol facts that are still useful for
implementation. Avoid adding session transcripts or dated progress logs here.

## Cursor Ingress

- Cursor sends OpenAI-compatible requests to `/v1/chat/completions`.
- The body is Responses-shaped: input items live in `input`, not `messages`.
- Cursor model names arrive as native-looking ids such as `gpt-5.4`,
  `gpt-5.5`, `claude-opus-4-7`, and `claude-haiku-4-5-20251001`.
- Cursor request ids and conversation ids live in metadata when present.
- Product tools include `Subagent`, `SwitchMode`, `AskQuestion`, `CreatePlan`,
  `ApplyPatch`, `CallMcpTool`, and `FetchMcpResource`.
- Mode and request-path semantics should be derived once in `cursor/`, not
  inferred later from prompt text.
- Cursor appears to preflight available models via `/v1/models`, build a local
  capability entry, decide whether to summarize/compact locally, and only then
  send `/v1/chat/completions` with the selected context payload.
- Cursor does not forward the MAX Mode / 1m-context toggle state in the
  `/v1/chat/completions` body. Live request comparisons only showed the model
  id plus metadata such as `cursorConversationId`; the selected context length
  must be inferred from Cursor behavior and `/v1/models` traffic.
- The visible Cursor "API usage limit" and "User API key rate limit exceeded"
  messages are fallback UI messages. Treat them as "Clyde or the upstream
  provider did not produce a Cursor-acceptable response" until Clyde logs prove
  a real 429 or quota event.

## Codex Provider

- Live Codex transport is direct websocket:
  `wss://chatgpt.com/backend-api/codex/responses`.
- Continuation is same-connection only. Cross-process or cross-connection
  `previous_response_id` reuse with `store: false` is not supported upstream.
- Clyde should keep the per-conversation websocket session cache and chain
  `previous_response_id` only inside that cached connection.
- Codex identity headers include `originator`, `x-codex-turn-metadata`,
  `x-codex-installation-id`, and `x-codex-window-id`.
- Current open validation is about turn metadata parity, longer-session reuse,
  prompt-cache behavior, and token-drop evidence, not the old fingerprint
  matcher.
- Current long-turn logs show same-connection reuse through
  `adapter.codex.ws_session.taken` / `put` and high prompt-cache reads around
  160k-186k prompt tokens. Generation ids are still absent when Cursor only
  supplies `cursorConversationId` and `cursorRequestId` metadata.

## Anthropic Provider

- Claude Code native compatibility targets Anthropic-shaped endpoints via
  `ANTHROPIC_BASE_URL`, not an OpenAI base-url path.
- Clyde exposes native `/v1/messages` and `/v1/messages/count_tokens` beside
  the OpenAI facade.
- The observed basic `claude -p` path called `/v1/messages?beta=true` and did
  not call `/v1/models` or `/v1/messages/count_tokens`.
- `/v1/messages/count_tokens` is intentionally a typed `not_supported` stub
  until live Claude Code traffic proves a fuller implementation is needed.
- Native ingress should accept Claude-compatible ids and must not remap
  `opus`, `sonnet`, or `haiku` requests to GPT aliases.
- Native ingress should resolve Clyde aliases to upstream Claude ids before
  calling Anthropic, while preserving raw `claude-*` ids when Claude Code sends
  them directly.

## MITM Baselines

- The daemon-owned MITM listener is the intended source of truth for rolling
  local baselines under XDG state.
- Always-on capture files may mix providers, so baseline refresh needs typed
  provider filtering before extracting snapshots.
- Claude HTTP traffic can refresh Snapshot v2 baselines directly from the
  always-on capture store.
- Codex CLI still uses CONNECT tunnel mode in the current in-process proxy, so
  tunneled payload bytes are not available for rolling baseline refresh without
  a deeper interception layer.

## Error And Notice Handling

- Anthropic 429 maps to a retryable upstream error, but Cursor/OpenAI ingress
  must return a canonical OpenAI invalid-request envelope with the upstream
  reset text in `error.message`, following the Codex context-window error
  pattern. Do not expose it as OpenAI
  `rate_limit_error` / `rate_limit_exceeded`, because Cursor treats that as
  BYOK key exhaustion and replaces the message with fallback UI.
- Anthropic 200 responses with overage or rate-limit warning headers are
  successful responses with notices, not failures.
- Streaming errors after headers are committed should be surfaced as SSE error
  frames plus `[DONE]`, not as late HTTP JSON responses.

## Context And Capability Notes

- The `/v1/models` surface needs to report context budgets that match observed
  provider behavior closely enough for Cursor to make good decisions.
- Live `/v1/models` currently reports `1000000` context for 1m Opus aliases and
  `200000` for non-1m Opus aliases. Current long-turn logs are still below the
  advertised 1m budget, so they do not prove whether Cursor auto-summarization
  should already have triggered for 1m aliases.
- GPT/Codex aliases must avoid Cursor's native catalog assumptions where
  possible. Native-looking `gpt-5.5` was observed in Cursor as a large-context
  model even when Clyde advertised `272000` through `/v1/models`.
- `gpt-5.4` is currently treated as the 1m-capable GPT alias in Clyde. `gpt-5.5`
  should be treated as about `272000` input context unless fresh upstream
  evidence proves otherwise.
- Clyde-specific GPT/Codex aliases are now declared under `[adapter.codex.models]`
  and must include an effort segment. For example, `clyde-codex-5.5-high`
  advertises `272000` context and normalizes upstream to `gpt-5.5`, while
  `clyde-gpt-5.4-1m-medium` advertises `1000000` context and normalizes
  upstream to `gpt-5.4`.
- Cursor still sent oversized `clyde-gpt-5.5` requests before bare non-effort
  aliases were removed, and Cursor later mangled `gpt-5.5` / `gpt-5-5`-looking
  model ids. Current GPT 5.5 aliases use the `clyde-codex-5.5-*` prefix to avoid
  that native-catalog path. Recent failing turns had about `1697776`
  request-body bytes, `1169` input items, and `previous_response_id` present;
  Clyde resolved the request to `272000` before forwarding upstream to
  `gpt-5.5`.
- Therefore `/v1/models` metadata is necessary but not sufficient for reliable
  protection. Clyde needs adapter-side preflight for known context-window
  overflows so it can reject before opening an upstream Codex turn, and the
  rejection shape should be Cursor-compatible enough to trigger retry,
  compaction, or a clear user-visible error.
- `CLYDE-158` tracks context-window mismatch handling.
- `CLYDE-163` tracks Cursor auto-summarization not engaging for Clyde adapter
  models.
- `CLYDE-169` tracks making model mappings, alias exposure, effort tiers, and
  context budgets fully config-driven with no GPT/Codex hard-coded details.
