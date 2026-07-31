# Logging Overview

Clyde request logging uses one typed request-event contract in `internal/logevent`. The contract gives adapter chat traffic and MITM IDE backend traffic shared identity fields, leg fields, outcome fields, provider facet interface, and sink names.

Use `clyde logs inventory --json` as the first discovery step for live log locations. Use `clyde logs inventory --deep --json` when the indexed view needs to be verified against the filesystem.

Logs carry request metadata only, never raw prompts, bodies, credentials, cookies, or tokens. Full request and response bodies live only in the SQLite capture store at `mitm/capture.db`. See `payload-policy.md`.

Request traffic goes through the central `logevent.Emitter` and is reviewed against the required legs in `request-paths.md`.

Start here, then use these references:

- `contract.md` for canonical fields and event names.
- `request-paths.md` for adapter and MITM leg sequences.
- `sinks.md` and `inventory.md` for log locations and discovery.
- `provider-facets.md` for typed provider-specific metadata.
- `layout.md` for stable on-disk layout classes.
- `rotation-cleanup.md` for cleanup behavior.
- `operations.md` for following a request by ID.
- `metrics.md` for retained daemon metrics history.
