# Logging Contract

The canonical request event package is `internal/logevent`.

Request traffic emits `logging.request.leg` through `logevent.Emitter`. A request story that finishes without all required legs emits `logging.request.incomplete`, unless it observed the early failure leg `request_error`.

Shared identity fields:

- `trace_id`
- `span_id`
- `parent_span_id`
- `request_id`
- `cursor_request_id`
- `cursor_conversation_id`
- `cursor_generation_id`
- `upstream_request_id`
- `upstream_response_id`
- `chat_key`
- `chat_key_source`
- `chat_root_key`
- `chat_branch_key`
- `session_id`

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

Shared payload fields:

- `payload_summary`
- `payload_fields`
- `payload_removed`

Sink field:

- `sinks`

Provider facet fields:

- `codex`
- `anthropic`
- `mitm`

The supported surfaces are `adapter_chat` and `mitm_ide_backend`. The supported route families are `openai_compatible`, `native_anthropic`, and `mitm_proxy`.

The supported phases are `started`, `completed`, and `failed`. The supported status values are `ok` and `error`.

Removed legacy fields and event shapes:

- Normal JSONL logs do not use `body`, `body_b64`, or `body_truncated`.
- The user-facing `logging.body` and `mitm.body_mode` payload config surfaces are removed.
- Request traffic does not add compatibility aliases for old event names or field names.
