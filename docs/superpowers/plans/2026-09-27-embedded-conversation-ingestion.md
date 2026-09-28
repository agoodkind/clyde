# Clyde append ingestion plan

The [shared coordination plan](2026-09-27-shared-search-coordination.md) is the sole execution order and acceptance ledger. The [Clyde design](../specs/2026-09-27-embedded-search-design.md) defines the product and library contract. This lane owns only the files and tests below.

## C2. Commit append only ingestion and recover it

Depends on: C1 and LMS L1 catalog and append semantics.

Files:

- Modify: [internal/daemon/conversation_semantic_sync.go](../../../internal/daemon/conversation_semantic_sync.go).
- Create: `internal/daemon/conversation_semantic_outbox.go`.
- Create: `internal/daemon/embedded_ingestion_recovery_test.go` for a real provider index, Milvus client, SQLite catalog, and outbox. C4 owns the later CLI and MCP integration test.
- Modify: [internal/cli/daemon/backfill.go](../../../internal/cli/daemon/backfill.go) for the new explicit operator behavior.
- Modify: [internal/daemon/initial_index.go](../../../internal/daemon/initial_index.go) for first-install ingestion from an existing raw cache.
- Modify: [internal/daemon/hard_reset.go](../../../internal/daemon/hard_reset.go) for explicit reset scope without automatic committed occurrence deletion.

Behavior:

- Keep the existing provider index, growing artifact deferral, current 8 MiB raw batch target, failed load reporting, and freshness accounting. C4 exposes the raw batch target as configuration. A large raw artifact may exceed the target when loaded alone; measure that peak separately. Replace manifest needed-set and full owner replacement with a durable outbox of immutable occurrence batches. Store batch ID, source identity, policy version, occurrence IDs, selected row counts, and library receipt.
- Write the outbox before append. Stage only new prepared parts for one owner generation. At EOF, call `CommitGeneration` with `GenerationSeal{RowCount, ManifestHash}` and mark delivery complete only after its receipt identifies the committed uint64 generation. Replay an unacknowledged batch after restart. Replayed stable row keys must have byte-identical immutable fields. The library publishes all verified staged parts atomically. Appended messages create only their new occurrences; a truncated or lost source preserves old rows. Metadata-only changes use `ReprojectScalars` with exact row keys and perform zero vector writes.
- Distinguish failed source reads from an empty selected projection. Suppress repeated unchanged failures without claiming the source was indexed. Report source reading, projection, embedding, metadata persistence, and searchable completion separately.
- Call `ReprojectScalars` for previously indexed owners when archive, workspace, provider, or subagent classification changes. Supply a monotonic `ProjectionOrder`, an idempotency token, and mutable declared values keyed by exact stable row keys. Do this even when the current content admission policy excludes that owner. The operation writes no vectors and does not load excluded transcript fields. An absent source retains its last accepted metadata.
- Make old delete and scalar backfill commands explicit about their new semantics. Never translate a routine sync or backfill into automatic occurrence deletion. Reject commands that request committed occurrence deletion.

Steps:

1. Add a Clyde owned SQLite outbox under the configured state directory with versioned schema and WAL. Transactionally record intent before calling the library. Keep it distinct from the MITM capture database and the library's occurrence catalog.
2. Adapt the sync worker to C1's occurrence preparation and the library append facade. Keep its rotating batch cursor and retry pacing. Do not treat a missing raw record as a delete signal.
3. Preserve the existing first-install semantic indexing path when a raw cache already exists. A missing semantic catalog starts ingestion from loaded raw index records without another provider parser or scan. Routine hard reset preserves committed occurrence catalogs and vector pools.
4. Stop and restart the ingestion worker at crash points before append, after library commit, and before receipt acknowledgment. Compare committed receipts and catalog row counts after each restart. C4 verifies ranked search through CLI and MCP after runtime wiring.

Verification:

- Run: `go test -tags live ./internal/daemon -run '^TestEmbeddedIngestionRecovery$' -count=1` in the supported pinned source workspace with the required Milvus service.
- Expect: An unchanged second pass sends zero occurrence rows and performs zero embeddings and zero vector writes. A restart replays only unacknowledged work. Source loss preserves the last accepted occurrence. No routine path deletes occurrences.
