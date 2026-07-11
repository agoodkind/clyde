# Logging Contract

The canonical request event package is `internal/logevent`.

Request traffic emits `logging.request.leg` through `logevent.Emitter`. A request story that finishes without all required legs emits `logging.request.incomplete`, unless it observed the early failure leg `request_error`.

Shared identity fields:

- `trace_id`
- `span_id`
- `parent_span_id`
- `request_id`
- `upstream_request_id`
- `upstream_response_id`
- `chat_key`
- `chat_key_source`
- `chat_root_key`
- `chat_branch_key`
- `session_id`

Provider-owned identity attributes can add safe flat fields such as `cursor_request_id`, `cursor_conversation_id`, and `cursor_generation_id`. Cursor-owned ingress and MITM contracts emit those Cursor fields; `internal/logevent` stores them as provider-owned attributes without interpreting their keys.

Shared path fields:

- `surface`
- `route_family`
- `path`
- `method`
- `host`
- `backend`
- `provider`
- `upstream_url`
- `leg`
- `phase`

Shared outcome fields:

- `status`
- `status_code`
- `error_code`
- `error_message`
- `duration_ms`
- `bytes_in`
- `bytes_out`

Sink field:

- `sinks`

Provider facet fields:

- `codex`
- `anthropic`
- `cursor`
- `mitm`

The supported surfaces are `adapter_chat` and `mitm_ide_backend`. The supported route families are `chat_compatible`, `provider_native`, and `mitm_proxy`.

The supported phases are `started`, `completed`, and `failed`. The supported status values are `ok` and `error`.

The request log carries the identity, path, outcome, sink, and facet fields above. Full request and response bodies live only in the SQLite capture store at `mitm/capture.db`.

The `/v1/responses` route serves the OpenAI Responses API on the OpenAI-compatible route family. It traces its handler span as `adapter.openai.responses` and logs under the shared `adapter.chat.*` and `adapter.http.*` concerns. It does not emit the typed `logging.request.leg` request story today, so it does not populate the leg field or trigger `logging.request.incomplete`.
