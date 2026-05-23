# Payload Policy

Normal JSONL request logs use one fixed filtered inline payload view. The policy is implemented by `logevent.FilterPayload`.

`payload_summary` records content type, body type, SHA-256 hash, total byte count, JSON field count, and array item count where available.

`payload_fields` records retained JSON fields as typed path, value, and byte count entries. Unknown non-context JSON fields are retained, so safe metadata remains visible without adding per-provider allowlists.

`payload_removed` records removed context fields as path, reason, byte count, and item count entries. The context field set includes messages, message, input, inputs, tools, tool, functions, function, instructions, prompt, prompts, conversation, chats, context, and system.

Raw capture is separate from the inline payload view. `logging.raw_capture.enabled = true` permits raw request and response sidecar files where a sink supports them. `logging.raw_capture.enabled = false` prevents raw payload sidecar files. Normal process logs, concern logs, request logs, inventory output, and capture indexes do not inline raw payload bodies.

A non-JSON body is summarized as bytes. A top-level JSON array is treated as context and removed as one payload record.
