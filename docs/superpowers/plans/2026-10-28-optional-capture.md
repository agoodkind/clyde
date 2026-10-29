# Optional capture implementation plan

Use executing-plans to implement this plan.

## Goal

Run MITM injection and baseline learning without saving routine request and response exchanges. Allow temporary full capture with an absolute expiration. Keep compact operational logs and the existing retention limits.

## Design

Add `mitm.capture_store.record_until` as an optional RFC3339 timestamp. An empty value disables exchange recording. A future value enables recording until that time. Expiration applies to HTTP and websocket exchanges without a restart. Reloading cannot extend the deadline. Keep the shared SQLite store open for baseline reads, learning, and retention.

The capture store rejects exchange records after expiration. Callers also skip recording-only buffers and decoding when recording is off. Preserve response forwarding, usage parsing, injection, and baseline processing.

Use info logging for operational diagnostics. Extra provider sidecars, chat transcripts, and the inventory index remain optional. Keep operational and audit logs. Retention expires existing captures independently of the recording setting.

## Constraints

- Preserve active streams during deployment through listener inheritance.
- Preserve provider conversations, search caches, backups, credentials, and learned baselines.
- Keep the default capture retention of 5,000,000,000 bytes and seven days.
- Run `make build`, fix failures within scope, satisfy GitHub rules, merge, and deploy through the existing listener handoff.

## Tasks

- [ ] Add the timestamp configuration, map it into the daemon store, and enforce the recording deadline before queueing exchange records. Use real SQLite tests for disabled recording, expiration, baseline operations, and configuration rejection.
- [ ] Gate recording-only collection in MITM HTTP, CONNECT, websocket, adapter ingress, and provider egress. Preserve usage parsing. Verify actual forwarding and injection while the database does not gain exchange rows.
- [ ] Update the capture and logging instructions.
- [ ] Run focused tests and `make build`. Review the complete diff independently. Create a ready PR, satisfy the active merge contract, merge, and deploy the merged revision.
- [ ] Verify the deployed revision, service readiness, injection, stopped exchange recording, and baseline preservation. Clean up the task branch and worktree through cleanup-git.

## Review focus

- A stream that completes after expiration must not persist its collected exchange.
- Adapter ingress settings must not bypass the global recording deadline.
- Websocket recording must expire without disabling relay or injection.
- Disabled capture must not disable baseline learning or retention.
- A malformed timestamp must fail configuration loading with an actionable error.
