# Provider Facets

Provider-specific metadata belongs in typed facets on shared request events.

Codex facet fields:

- `model`
- `effort`
- `reasoning_summary`
- `previous_response_id`
- `stream_event_count`
- `retry_attempt`

Anthropic facet fields:

- `model`
- `thinking_enabled`
- `stop_reason`
- `stream_event_count`
- `retry_attempt`

MITM facet fields:

- `concern`
- `request_content_type`
- `response_content_type`
- `capture_path`
- `raw_request_path`
- `raw_response_path`

Provider facets must not carry raw prompts, request bodies, response bodies, credentials, cookies, or tokens. Provider sidecars may keep safe structural summaries such as counts, hashes, model names, retry attempt numbers, and upstream response identifiers when those identifiers are already part of the shared identity contract.

When provider-specific information is useful but unsafe, do not add it to the facet. Record a safe count, hash, boolean, or explicit omission reason instead.

## Codex provider sidecar

Two Codex events still write through the provider sidecar logger to `codex.jsonl` rather than being folded into a typed facet on a shared `logging.request.leg` event:

- `adapter.codex.response.received` records per-WebSocket frame details when the Codex Responses transport runs over a WebSocket. The event captures the raw frame type, frame sequence number, and websocket close-code classification. Those fields are transport-level diagnostics that do not map to any logical leg of the adapter chat request story, so promoting them to a facet would either widen `ProviderFacets.Codex` with WebSocket-specific fields that have no meaning for the SSE transport or drop fidelity by averaging across frames.
- `adapter.codex.upstream_sse.aggregate` records an end-of-stream SSE fingerprint with typed substructures for tools, reasoning, and emitted message types. The aggregate is one record per upstream Codex response. It runs independently of the leg state machine because the fingerprint is computed after the SSE reader closes, and several of its fields (per-tool counts, per-event-type counts) cannot be flattened into a single facet without losing the per-type breakdown.

Both events stay opt-in for Codex-specific debugging. They do not gate the typed leg sequence and they do not stand in for any required leg. New Codex telemetry should prefer a facet field on `ProviderFacets.Codex`; choose the sidecar only when the data is transport-shape-specific or a per-stream aggregate that has no natural leg to attach to.
