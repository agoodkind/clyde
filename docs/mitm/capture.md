# MITM capture store

The MITM proxy records every intercepted exchange in the SQLite capture store at
`mitm/capture.db` under the log root. Each exchange is one row in `requests`
with its full decoded request and response bodies in a side table `bodies`,
keyed by `request_row_id`. The schema lives in
`internal/mitm/capture/schema.sql`.

## How an exchange is tagged

Each `requests` row carries the metadata you query by:

- `client`: the listener id that captured it, for example `app.cursor`. One
  loopback port maps to one application, so `client` names the application. See
  [listeners.md](listeners.md).
- `host`: the destination domain.
- `provider`: a metadata label only. It is `unspecified` when no registered
  provider claimed the host.
- `concern`: an auto-derived grouping label. The classifier lives in
  `internal/mitm/raw_capture.go`.

## Capture is by domain, not an allowlist

A listener captures every host reached on its port, not only the hosts a
provider claims. The provider classification is metadata and never decides
whether an exchange is captured. A host that no provider claims is still
terminated, captured, and tagged `provider = unspecified`, organized by its
`host` domain. This is why a store-wide text search finds a phrase regardless of
which host carried it.

## Search the store for a phrase

Scan the raw body bytes with a hex match, joined from `bodies` to `requests`:

```sql
SELECT r.id, r.client, r.provider, r.host, r.path, r.status, b.which
FROM bodies b
JOIN requests r ON r.id = b.request_row_id
WHERE instr(hex(b.data), hex('YOUR_PHRASE')) > 0
ORDER BY r.ts DESC;
```

Use `instr(hex(b.data), hex('YOUR_PHRASE'))`, not
`CAST(b.data AS TEXT) LIKE '%YOUR_PHRASE%'`. A body is often a protobuf or a
decoded binary blob, and `CAST(... AS TEXT)` stops at the first NUL byte, so it
silently misses a phrase that sits after a NUL. The hex match scans the whole
blob.

Do not pre-filter by `client`, `provider`, or `host` while you are locating a
phrase. The traffic may arrive on a different listener or under
`provider = unspecified`, so a filter can hide the row. Add filters only after
the raw scan shows where the phrase landed.

The bodies hold raw prompts, tokens, cookies, credentials, and response
payloads. Keep query output that contains them out of tickets and chat.
