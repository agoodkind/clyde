# Search MITM captures

Enable a temporary recording period through the [capture settings](../logging/config.md)
before reproducing a problem. Each recorded exchange creates one row in `requests`
and stores its raw request and response bytes in `bodies`, keyed by
`request_row_id`. Provider decoders can also create decoded representations and
tool events.

## Select exchange metadata

Use these fields to filter recorded exchanges:

- `client` identifies the listener, such as `app.cursor`, or the adapter egress client.
- `host` identifies the destination domain.
- `provider` identifies the registered provider, or `unspecified` when a provider does not claim the host.
- `concern` groups exchanges by request classification.

During a recording period, a listener records exchanges for every host it
intercepts. Provider classification assigns metadata rather than selecting
which exchanges to record.

## Search body bytes

Scan raw body bytes with a hex match joined from `bodies` to `requests`:

```sql
SELECT r.id, r.client, r.provider, r.host, r.path, r.status, b.which
FROM bodies b
JOIN requests r ON r.id = b.request_row_id
WHERE instr(hex(b.data), hex('YOUR_PHRASE')) > 0
ORDER BY r.ts DESC;
```

Use `instr(hex(b.data), hex('YOUR_PHRASE'))` for binary bodies. SQLite text
conversion stops at the first NUL byte. The hex match scans the whole blob.

Start without a `client`, `provider`, or `host` filter. Add those filters after
the raw scan identifies matching exchanges.

Captured bodies can contain private conversation content. Keep query output
that contains that content out of tickets and chat.
