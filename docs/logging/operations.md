# Logging Operations

Start every logging investigation with inventory.

```bash
clyde logs inventory --json
```

Use a deep scan when counts or largest files need exact filesystem confirmation.

```bash
clyde logs inventory --deep --json
```

Inventory reports whichever state root the environment points at, so a plain run
finds the deployed daemon's logs even while a sandbox is running. To inspect a
sandbox instead, prefix the command with the environment that sandbox printed at
startup.

Follow one request with shared identity fields. Prefer these fields when present:

- `trace_id`
- `request_id`
- `upstream_request_id`
- `upstream_response_id`
- `chat_key`
- `session_id`

Cursor identity fields differ by surface, so match the field shape to the traffic surface before querying. Adapter BYOK (OpenAI-compatible) Cursor traffic emits the flat fields `cursor_request_id`, `cursor_conversation_id`, and `cursor_generation_id`. Cursor MITM IDE backend traffic nests its Cursor identity under a `cursor` group object with the keys `request_id`, `conversation_id`, `generation_id`, `session_id`, and `original_request_id`. When searching the MITM wire logs at `logs/providers/mitm/wire.jsonl`, query the nested `cursor.*` keys rather than the flat `cursor_*` names.

For Cursor chat issues, identify whether the traffic is adapter BYOK or MITM IDE backend traffic, then apply the rule for that traffic surface. Adapter BYOK uses the OpenAI-compatible adapter route family. Cursor IDE backend traffic traverses the MITM proxy. See `request-paths.md` for the shared logging leg model.

For missing logs, check for `logging.request.incomplete`. That event names the required legs, observed legs, missing legs, and last observed phase in the JSON fields `expected_legs`, `observed_legs`, `missing_legs`, and `last_phase`.

For sensitive data review, full request and response bodies live only in the SQLite capture store at `mitm/capture.db`; the wire logs carry metadata. Keep raw prompts, tokens, cookies, credentials, and response bodies out of tickets and chat.
