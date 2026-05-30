# Provider Facets

Provider-specific metadata belongs in typed `logevent.Facet` values on shared request events. Generic logging code emits each facet through the `logevent.Facet` interface, and provider packages own the concrete facet structs and field names.

Codex facet fields:

- `model`
- `effort`
- `reasoning_summary`
- `transport`
- `service_tier`
- `previous_response_id`
- `prompt_cache_key_present`
- `input_count`
- `tool_count`
- `stream_event_count`
- `retry_attempt`

Anthropic facet fields:

- `model`
- `thinking_enabled`
- `stop_reason`
- `stream_event_count`
- `retry_attempt`

Cursor facet fields:

- `request_id`
- `conversation_id`
- `generation_id`
- `original_request_id`
- `session_id`

MITM facet fields:

- `concern`
- `request_content_type`
- `response_content_type`
- `capture_path` (deprecated, always empty: the struct field is retained but every emit site sets it to the empty string)
- `raw_request_path`
- `raw_response_path`

Provider facets must not carry raw prompts, request bodies, response bodies, credentials, cookies, or tokens. Provider sidecars may keep safe structural summaries such as counts, hashes, model names, retry attempt numbers, and upstream response identifiers when those identifiers are part of the shared identity contract.

When provider-specific information is useful but unsafe, do not add it to the facet. Record a safe count, hash, boolean, or explicit omission reason instead.

## Cursor TLS-intercept

Cursor TLS-intercept HTTP traffic emits the shared `logging.request.leg` sequence used by Claude and Codex MITM HTTP paths. The transport-specific code in `internal/mitm/connect_tunnel.go` owns certificate impersonation, raw sidecar paths, and hook dispatch. Decrypted HTTP requests feed `recordHTTPCapture(...)`, defined in `internal/mitm/proxy.go`, which orchestrates the leg-emit helpers `emitHTTPLogLeg` and `emitHTTPPayloadLeg` in `internal/mitm/request_logging.go`.

There is no separate capture index file. The dedicated `capture.jsonl` file and the `mitm_capture` sink are eliminated. MITM wire legs, both the HTTP path and the websocket path, emit on the `providers.mitm.wire` concern and land in `logs/providers/mitm/wire.jsonl` through the gklog concern router, with one non-dropping blocking `FileJSON` writer behind the async logger.

The shared MITM facet sets `capture_path` to empty at every emit site, so that field is deprecated and always empty. The facet still carries `raw_request_path` and `raw_response_path` as the live per-request raw sidecar references, populated only when `logging.raw_capture` is enabled.

Cursor TLS-intercept HTTP entries keep the original `traceparent` header and write a derived `trace_id` alias when the header parses cleanly. Both ride the `providers.mitm.wire` concern log rather than a separate `capture.jsonl`.

## Codex provider sidecar

The Codex provider sidecar logger writes these events to `codex.jsonl`:

- `adapter.codex.response.received` records per-WebSocket frame details when the Codex Responses transport runs over a WebSocket. The event captures the raw frame type, frame sequence number, and websocket close-code classification. Those fields are transport-level diagnostics that do not map to any logical leg of the adapter chat request story.
- `adapter.codex.upstream_sse.aggregate` records an end-of-stream SSE fingerprint with typed substructures for tools, reasoning, and emitted message types. The aggregate is one record per upstream Codex response. It runs independently of the leg state machine because the fingerprint is computed when the SSE reader closes, and several of its fields (per-tool counts, per-event-type counts) cannot be flattened into a single facet without losing the per-type breakdown.

These events are opt-in Codex-specific diagnostics. They do not gate the typed leg sequence, and they do not stand in for any required leg. Codex telemetry belongs in the Codex facet when the value describes one request-story leg. Codex telemetry belongs in `codex.jsonl` when the value describes transport frames or a per-stream aggregate.
