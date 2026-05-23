# Logging Operations

Start every logging investigation with inventory.

```bash
clyde logs inventory --json
```

Use a deep scan when counts or largest files need exact filesystem confirmation.

```bash
clyde logs inventory --deep --json
```

Follow one request with correlation fields. Prefer these fields when present:

- `trace_id`
- `request_id`
- `cursor_request_id`
- `cursor_conversation_id`
- `cursor_generation_id`
- `upstream_request_id`
- `upstream_response_id`
- `chat_key`
- `session_id`

For Cursor chat issues, identify whether the traffic is adapter BYOK or MITM IDE backend traffic before applying a rule. Adapter BYOK uses the OpenAI-compatible adapter route family. Cursor IDE backend traffic traverses the MITM proxy. See `request-paths.md` for the shared logging leg model.

For missing logs, check for `logging.request.incomplete`. That event names the required legs, observed legs, missing legs, and last observed phase.

For sensitive data review, inspect `payload_removed` and confirm context fields were removed. Do not paste raw prompts, tokens, cookies, credentials, or response bodies into tickets or chat.
