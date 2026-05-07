# Cursor error-shape research runbook

This runbook defines a repeatable way to test which OpenAI-family error
envelopes Cursor renders, retries, or replaces with generic fallback UI. It is
research-only. Do not change production error mapping from this document alone.

## Scope

The artifact has two parts:

- Real Clyde correlation for the current production shape.
- A local OpenAI-compatible probe server that returns selected JSON and SSE
  error shapes to real Cursor without changing Clyde production code.

Use Cursor itself for every matrix row. `curl` is useful only to prove the local
response body before Cursor sees it.

## Probe server

Run the docs-adjacent probe server from the repository root:

```sh
go run ./docs/adapter-refactor/cursor-error-shape-probe -addr localhost:48761 -shape json-current-upstream-failed
```

Configure Cursor BYOK for one run:

- Base URL: `http://localhost:48761/v1`
- API key: any non-empty local placeholder
- Model: `clyde-error-shape-probe`

Change the active shape without restarting the server:

```sh
curl -fsS -X POST 'http://localhost:48761/__probe__/shape?shape=json-upstream-unavailable'
curl -fsS 'http://localhost:48761/__probe__/shape'
```

The probe server exposes only `/v1/models`, `/v1/chat/completions`, and
`/__probe__/shape`. It does not import Clyde adapter code, does not register
with the daemon, and does not alter production error selection.

## MITM capture

Capture two different network surfaces and label them separately:

- Clyde/probe response: direct response from Clyde or the local probe server to
  Cursor's configured OpenAI base URL.
- Cursor backend response: Cursor's later `AgentService/RunSSE` response from
  Cursor infrastructure to the app.

Use an HTTPS MITM that can see Cursor's backend traffic, and verify the capture
contains `AgentService/RunSSE` before treating it as evidence. The daemon-owned
Clyde MITM is for Clyde-launched provider CLI traffic and rolling provider
baselines. It is not proof of Cursor ingress or Cursor backend behavior.

For each row, record:

- Probe id: a unique string in the user prompt, for example
  `cursor-error-shape-20260506-json-current-upstream-failed`.
- Clyde/probe response: HTTP status, `Content-Type`, raw body, SSE frames, and
  any Clyde `request_id`, `cursor_request_id`, or `cursor_conversation_id`
  fields if the row uses real Clyde.
- Cursor backend response: `AgentService/RunSSE` status and the raw
  `ErrorDetails.debug.error` value from the MITM capture.
- Visible UI behavior: exact visible text, whether Cursor retries, whether it
  opens generic rate-limit chrome, whether the message appears in the chat
  transcript, and whether the generation can continue.
- Cursor version, Clyde git commit, probe server command, active shape, local
  time, and whether the request used streaming.

## Matrix

| Shape | Server command or shape selector | Expected Clyde/probe response to Cursor | Cursor evidence to record |
| --- | --- | --- | --- |
| Current production JSON shape | `json-current-upstream-failed` | HTTP 400 JSON: `error.type=invalid_request_error`, `error.code=upstream_failed` | `AgentService/RunSSE` `ErrorDetails.debug.error`, visible UI, retry behavior |
| Unavailable code variant | `json-upstream-unavailable` | HTTP 400 JSON: `error.type=invalid_request_error`, `error.code=upstream_unavailable` | Same fields as above |
| Temporary code variant | `json-temporarily-unavailable` | HTTP 400 JSON: `error.type=invalid_request_error`, `error.code=temporarily_unavailable` | Same fields as above |
| Spec-like server error | `json-503-server-error` | HTTP 503 JSON: `error.type=server_error`, `error.code=upstream_failed` | Same fields as above |
| 503 with current type | `json-503-invalid-request` | HTTP 503 JSON: `error.type=invalid_request_error`, `error.code=upstream_failed` | Same fields as above |
| Current SSE error plus DONE | `sse-error-done-current` | HTTP 200 SSE: `data: {"error": ...}` then `data: [DONE]` | Same fields as above, plus whether Cursor treats the stream as complete |
| Current SSE error without DONE | `sse-error-only-current` | HTTP 200 SSE: one `data: {"error": ...}` frame and then socket close | Same fields as above, plus whether Cursor hangs, retries, or finalizes |
| Delta then current SSE error | `sse-delta-error-done-current` | HTTP 200 SSE: assistant delta, `data: {"error": ...}`, then `data: [DONE]` | Same fields as above, plus whether partial assistant text survives |
| Named SSE error event | `sse-event-error-done-current` | HTTP 200 SSE: `event: error`, `data: {"error": ...}`, then `data: [DONE]` | Same fields as above, plus whether Cursor ignores the event name |

## Per-row procedure

1. Start or update the probe server with the selected shape.
2. Prove the local response with `curl` before using Cursor.
3. Start the MITM capture and confirm it sees Cursor backend traffic.
4. In real Cursor, send a prompt containing the probe id.
5. Save the Clyde/probe response bytes and Cursor backend
   `AgentService/RunSSE` bytes.
6. Write the visible UI result immediately while the screen still shows it.
7. Stop the capture and attach the capture path or redacted excerpt to the
   evidence table.

Example `curl` checks:

```sh
curl -i http://localhost:48761/v1/models
curl -i -X POST http://localhost:48761/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"clyde-error-shape-probe","messages":[{"role":"user","content":"cursor-error-shape-probe"}]}'
curl -N -X POST http://localhost:48761/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"clyde-error-shape-probe","stream":true,"messages":[{"role":"user","content":"cursor-error-shape-probe"}]}'
```

## Evidence table

Append new rows here. Keep raw prompt text, secrets, cookies, API keys, and
provider tokens out of this table.

| Date | Probe id | Shape | Clyde/probe response | Cursor `AgentService/RunSSE` `ErrorDetails.debug.error` | Visible Cursor UI | Interpretation |
| --- | --- | --- | --- | --- | --- | --- |
| 2026-05-06 | prior correlation, generation id not recorded here | `json-current-upstream-failed` equivalent | Clyde emitted HTTP 400 with `error.type=invalid_request_error` and `error.code=upstream_failed` | Cursor returned `ERROR_API_KEY_RATE_LIMIT` for the same generation | Cursor showed rate-limit style fallback UI | Correlation only. This suggests Cursor may have transformed or rejected Clyde's shape, but it does not prove which field caused the fallback because the full matrix was not run. |

## Interpretation rules

- Treat matching Clyde/probe and Cursor generation ids as correlation, not
  proof, until the row includes the raw Clyde/probe response, raw Cursor
  backend response, and visible UI text.
- Do not infer production mapping changes from a single row. Prefer a shape
  only after at least the JSON rows and the SSE rows have been tested on the
  same Cursor build.
- If Cursor reports `ERROR_API_KEY_RATE_LIMIT`, verify whether Clyde actually
  returned an upstream 429 before calling it a real provider quota event.
- If the local probe and real Clyde disagree for the same nominal shape, record
  the byte-level difference before changing either the harness or production.
