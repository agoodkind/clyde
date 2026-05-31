# Payload Policy

Normal JSONL request logs use one fixed filtered inline payload view. The policy is implemented by `logevent.FilterPayload`.

`payload_summary` records content type, body type, SHA-256 hash, total byte count, JSON field count, and array item count where available.

`payload_fields` records retained JSON fields as typed path, value, and byte count entries. Unknown non-context JSON fields are retained, so safe metadata remains visible without adding per-provider allowlists.

`payload_removed` records removed context fields as path, reason, byte count, and item count entries. The context field set includes messages, message, input, inputs, tools, tool, functions, function, instructions, prompt, prompts, conversation, chats, context, and system.

MITM body capture is separate from the inline payload view. Normal process logs, concern logs, request logs, and inventory output do not inline raw payload bodies. Full decoded MITM request and response bodies persist to the SQLite capture store at `mitm/capture.db`, governed by `[mitm.capture_store]` retention. MITM wire legs land in the per-concern file `logs/providers/mitm/wire.jsonl` via the concerns sink and join to capture.db rows on `request_id`/`trace_id`.

A non-JSON body is summarized as bytes. A top-level JSON array is treated as context and removed as one payload record.
