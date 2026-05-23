# Logging Operations

Start every logging investigation with inventory.

```bash
clyde logs inventory --json
```

Use a deep scan when counts or largest files need exact filesystem confirmation.

```bash
clyde logs inventory --deep --json
```

Follow one request with shared identity fields. Prefer these fields when present:

- `trace_id`
- `request_id`
- `upstream_request_id`
- `upstream_response_id`
- `chat_key`
- `session_id`

Cursor-owned logs can also include `cursor_request_id`, `cursor_conversation_id`, and `cursor_generation_id`. Use those Cursor fields only for Cursor BYOK adapter traffic and Cursor MITM IDE backend traffic.

For Cursor chat issues, identify whether the traffic is adapter BYOK or MITM IDE backend traffic, then apply the rule for that traffic surface. Adapter BYOK uses the OpenAI-compatible adapter route family. Cursor IDE backend traffic traverses the MITM proxy. See `request-paths.md` for the shared logging leg model.

For missing logs, check for `logging.request.incomplete`. That event names the required legs, observed legs, missing legs, and last observed phase.

For sensitive data review, inspect `payload_removed` and confirm that it records context field removals. Do not paste raw prompts, tokens, cookies, credentials, or response bodies into tickets or chat.
