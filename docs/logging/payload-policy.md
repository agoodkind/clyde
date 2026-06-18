# Payload Policy

Normal JSONL request logs carry no payload view. Request and response legs record identity, path, outcome (status, byte counts, duration), and typed facets only. No field on a log line restates request or response body content, in whole, in summary, or as a hash.

Full request and response bodies persist to the SQLite capture store at `mitm/capture.db`, governed by `[mitm.capture_store]` retention. MITM wire legs land in the per-concern file `logs/providers/mitm/wire.jsonl` via the concerns sink and join to capture.db rows on `request_id`/`trace_id`. Adapter egress bodies persist to the same store, tagged by the egress client.

Process logs, concern logs, request logs, and inventory output never inline raw payload bodies.
