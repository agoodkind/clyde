# Logging Overview

Clyde request logging uses one typed request-event contract in `internal/logevent`. The contract gives adapter chat traffic and MITM IDE backend traffic the same identity fields, leg fields, outcome fields, payload view, provider facets, and sink names.

Use `clyde logs inventory --json` as the first discovery step for live log locations. Use `clyde logs inventory --deep --json` when the indexed view needs to be verified against the filesystem.

Normal JSONL logs never inline raw prompts, messages, tool schemas, response bodies, credentials, cookies, or tokens. They carry the fixed filtered payload view described in `payload-policy.md`. Full raw payload sidecar files are controlled only by `logging.raw_capture.enabled`.

The hard cut removes the payload mode ladder, raw-body JSONL fields, and event aliases. Request traffic goes through the central `logevent.Emitter` and is reviewed against the required legs in `request-paths.md`.

Start here, then use these references:

- `contract.md` for canonical fields and event names.
- `request-paths.md` for adapter and MITM leg sequences.
- `sinks.md` and `inventory.md` for log locations and discovery.
- `provider-facets.md` for typed provider-specific metadata.
- `layout.md` for stable on-disk layout classes.
- `rotation-cleanup.md` for cleanup behavior.
- `operations.md` for following a request by ID.
