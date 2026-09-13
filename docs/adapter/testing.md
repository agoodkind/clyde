# Adapter Testing

Adapter tests exercise public HTTP boundaries with local upstream servers and
assert the bytes, headers, and lifecycle state a client observes.

## Native Responses recovery

The native recovery test drives an encrypted v2 compaction response, the next
regular response, and the following natural resend. It verifies that the
compaction response stays byte-identical, the pending transcript is injected
once upstream, the regular response receives one tag, and the natural resend
does not receive another forced mutation.

The coverage also keeps generic Responses projection, v1 summary compaction,
unknown compaction implementations, failed responses, malformed input,
compression, expiry, and concurrent sessions on their separate paths. Capture
tests confirm that request and response copies redact credentials while native
forwarding preserves the intended wire data.
