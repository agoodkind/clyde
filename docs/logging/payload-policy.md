# Payload Policy

Request and response legs log identity, path, outcome (status, byte counts, duration), and typed facets. Full request and response bodies live only in the SQLite capture store at `mitm/capture.db`, governed by `[mitm.capture_store]` retention.

MITM wire legs land in `logs/providers/mitm/wire.jsonl` under the `providers.mitm.wire` concern and join to capture.db rows on `request_id`/`trace_id`. Adapter egress bodies persist to the same store, tagged by the egress client.
