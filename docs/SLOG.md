# SLOG contract

Canonical structured-logging field set for Clyde adapter events. Update this
file when fields change so AGENTS.md "Debugging and logs" stays accurate.

## adapter.chat.completed

Emitted once per OpenAI-compatible turn at the end of dispatch. Phase A2
unified the token shape across providers. The canonical token fields are:

- `prompt_tokens` (int): upstream-reported input tokens.
- `completion_tokens` (int): upstream-reported output tokens.
- `cache_read_tokens` (int): tokens served from prompt cache.
- `cache_creation_tokens` (int): tokens that wrote new prompt-cache
  entries. Anthropic-only on the wire; Codex always reports `0` here.
- `cache_creation_reported` (bool): whether the upstream contract
  exposes a cache-creation count. `true` for Anthropic, `false` for
  Codex per `research/codex/codex-rs/codex-api/src/sse/responses.rs`.
  Consumers that compute cache-write rates must filter on this flag to
  avoid treating Codex `0` as a real zero.
- `derived_cache_creation_tokens` (int): adapter-derived estimate when
  the upstream did not report a count.

The legacy `tokens_in` and `tokens_out` field names emitted by earlier
versions of `adapter.chat.completed` were removed in Phase A2. Operators
consuming these records must read the canonical names listed above.

The dedicated `adapter.cache.usage` event was retired in the same phase.
Cache token fields are now carried directly on `adapter.chat.completed`.
