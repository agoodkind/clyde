# SLOG contract

Clyde request logging uses one typed event contract in `internal/logevent`. Normal JSONL logs always carry a filtered inline payload view, and raw bodies are only written to raw-capture sidecar files when `logging.raw_capture.enabled = true`.

## Request Events

Request-path events use `logging.request.leg` with these shared fields:

- `trace_id`, `span_id`, `parent_span_id`, `request_id`, upstream identifiers, and chat partition identifiers when they are known.
- `surface`, `route_family`, `path`, `method`, `host`, `backend`, `provider`, `leg`, and `phase`.
- `status`, `status_code`, `error_code`, `error_message`, `duration_ms`, `bytes_in`, and `bytes_out` when available.
- `payload_summary`, `payload_fields`, and `payload_removed` for the fixed filtered inline payload view.
- Provider facets such as `codex`, `anthropic`, `cursor`, and `mitm` for safe provider-specific metadata.
- `sinks` to show the selected sink family for the event.

The required adapter chat legs are `adapter_ingress`, `adapter_payload`, `adapter_client_metadata`, `adapter_model_resolve`, `provider_send_started`, `provider_accepted`, `provider_response_started`, `provider_response_done`, `adapter_render`, and `adapter_client_egress`.

The required MITM IDE backend legs are `mitm_ingress`, `mitm_payload`, `mitm_upstream_send`, `mitm_upstream_start`, `mitm_forward`, `mitm_capture_index`, and `mitm_complete`. The `mitm_capture_index` leg is a request-story sequencing leg in that ordered series, not a write to a capture-index file.

Plain HTTP MITM traffic and Cursor TLS-intercept HTTP traffic both use this 7-leg MITM sequence. Decrypted HTTP requests enter `recordHTTPCapture(...)`, the shared MITM request recorder in `internal/mitm/proxy.go`, which sequences the per-leg helpers `beginHTTPLogRecorder`, `emitHTTPLogLeg`, `emitHTTPPayloadLeg`, `recordHTTPFailure`, and `completeHTTPLogRecorder` in `internal/mitm/request_logging.go`. Provider packages own TLS interception mechanics, and `internal/logevent` owns the request-story event shape. MITM request-story legs are written to the `providers.mitm.wire` concern file `logs/providers/mitm/wire.jsonl` through the gklog concern Router, because the dedicated `mitm/capture.jsonl` sink and the `mitm_capture` sink were removed.

A recorder emits `logging.request.incomplete` at warning level when a request story completes without every required leg.

## Payload Policy

The inline payload policy is fixed. It keeps metadata and non-context JSON fields, and it removes context-bearing fields such as messages, input, tools, functions, instructions, prompts, conversation, context, and system content. Removed fields are represented by path, reason, byte count, and item count.

`logging.raw_capture.enabled` controls whether raw request and response bodies can be written to local sidecar files. Normal process logs, concern logs, and request logs do not inline raw payload bodies; raw bodies go only to the `mitm/raw/<host>/` sidecars, and only when `logging.raw_capture.enabled` is true.

`logging.cleanup.enabled` controls whether retention cleanup may delete rotated logs. When disabled, cleanup policy resolves to `off`.

The canonical sink roster is the `LoggingSinkSpec` registry in `internal/config/logging_config.go`, validated by `IsKnownLoggingSink`, with per-sink `[logging.sinks.<name>]` blocks. Concern routing is `goodkind.io/gklog` `NewRouter`, with `ConcernRelPath` mapping dotted concerns to nested `.jsonl` paths.

## adapter.chat.completed

`adapter.chat.completed` remains the provider usage summary event emitted at the end of dispatch. The canonical token fields are:

- `prompt_tokens`: upstream-reported input tokens.
- `completion_tokens`: upstream-reported output tokens.
- `cache_read_tokens`: tokens served from prompt cache.
- `cache_creation_tokens`: tokens written to prompt-cache entries.
- `cache_creation_reported`: whether the upstream contract exposes a cache-creation count.
- `derived_cache_creation_tokens`: adapter-derived estimate when the upstream did not report a count.

The `tokens_in` and `tokens_out` aliases and the `adapter.cache.usage` event are not emitted.
