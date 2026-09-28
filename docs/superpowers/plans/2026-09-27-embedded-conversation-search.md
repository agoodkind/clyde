# Embedded conversation search implementation plan

## Goal

Clyde ingests and searches its selected conversation content through `goodkind.io/lm-semantic-search/library` in process. A caller owned Milvus client and a durable SQLite WAL catalog replace the LMS daemon RPC path. CLI and MCP search keep their existing result fields. The [design specification](../specs/2026-09-27-embedded-search-design.md) defines immutable occurrences, complete paging, and append only retention.

## Current behavior

[Conversation projection](../../../internal/daemon/conversation_semantic_documents.go) selects provider messages. [Semantic sync](../../../internal/daemon/conversation_semantic_sync.go) asks the LMS daemon for needed conversation IDs and sends documents. [Semantic runtime](../../../internal/daemon/conversation_semantic_runtime.go) dials LMS and retries registration. [Engine search](../../../internal/daemon/search_engine_hits.go) filters some engine hits after ranking and retries with a limit capped at 2,000. [Search paging](../../../internal/daemon/search_paging.go) converts those hits into the public page. [Search source](../../../internal/daemon/conversation_search_source.go) already isolates retrieval behind `conversationSearchSource`. [Conversation search types](../../../internal/conversation/list.go) define the CLI and MCP search options and result fields.

## Constraints and integration order

- Keep the registered `conversation.Parser` provider extension contract and `Index.LoadMessagesWithOptions`. Do not add another provider reader. Keep the original message index and `loadRules` for context reads.
- Keep the default chat and tool call selection. Keep tool output, thinking, archived conversations, and subagent conversations excluded by default. Make every selection lever typed and configurable.
- Preserve the shell search input. `gksyntax/shelldecomp.Parse(command, "/", "")` extracts bash program names and read and write targets. An opaque or empty parse keeps the raw command. Trimmed duplicate tokens appear once.
- Append immutable occurrences for provider messages. Use append mode with stable row keys and `ReprojectScalars` for mutable metadata. Do not delete a row for a normal pass, changed configuration, truncated artifact, or source loss. Keep existing vectors and occurrences. Routine new messages add only their own rows. A projection profile change uses a separately staged generation.
- Implement the library facade and build contract in LMS lane L0 before Clyde C1 compiles against it. C2 integrates after C1 and LMS L1. C3 preparation can start after L0; complete its query integration after C1 and LMS L3. C4 owns shared daemon startup and configuration files and joins C2 and C3. LMS L5 removes conversation RPCs only after the new Clyde path is released and supported clients are checked.
- Keep code lanes exclusive. C1 owns projection files, C2 owns feeder and durable state, C3 owns search files, and C4 owns configuration, runtime, and startup. Integrate through interfaces rather than editing another lane's files. Workers are not alone in the codebase; preserve edits from other lanes.
- No production reset or deployment belongs to this plan. A one-time corpus reset needs later research and an explicit migration decision.
- Public acceptance tests must launch a real daemon, use temporary provider artifacts, connect to real temporary SQLite and Milvus instances, and query through CLI and MCP. They must fail when a required dependency is unavailable. A skipped live test is not acceptance evidence.

## Proposed configuration contract

The following keys are the proposed `[conversation.semantic]` contract. An omitted library budget uses the library's stated default. Changes to these keys use `RouteReload`. A model, revision, dimension, normalization, analyzer, catalog path, or vector pool change creates a separately configured generation. Clyde builds and verifies the new generation before publishing it. Opening an existing catalog with a mismatched descriptor returns `ErrStoreMismatch`; the prior generation remains readable.

| Keys | Type and default | Library mapping or Clyde behavior |
| --- | --- | --- |
| `ingestion_enabled`, `search_enabled` | Existing booleans, false unless configured. | Independent write and read switches. |
| `collection_id`, `indexed_content` | Existing string and selector list. The current selector default remains chat plus tool calls. | Map collection ID to namespace and selectors to Clyde projection. |
| `indexed_providers`, `indexed_roles` | String lists. Empty means every supported provider and role. | Clyde content admission and generic scalar predicates. |
| `include_archived`, `include_subagents` | Booleans, false. | Clyde content admission and query predicates; metadata refresh still runs for already indexed owners. |
| `catalog_path`, `lock_path`, `pool_id` | Explicit paths and pool ID when enabled. | `StoreDescriptor.CatalogPath`, `LockPath`, `PoolID`. Existing configured values are never silently repurposed. |
| `milvus_address`, `milvus_database`, `milvus_collection` | Explicit endpoint and names for the Milvus profile. | Clyde creates the SDK client and injects `library/milvus`; the library does not close the client. |
| `embedding_model`, `embedding_revision`, `vector_dimension`, `normalization`, `analyzer_identity`, `query_instruction_prefix` | Explicit model identity, resolved revision, positive dimension, normalization identifier, analyzer identity, and optional prefix. | `StoreDescriptor` and `library.Config`. A descriptor mismatch fails before writes. |
| `max_batch_rows`, `max_batch_bytes` | Positive integers; defaults 256 and 8 MiB. | `library.Config.MaxBatchRows` and `MaxBatchBytes`. |
| `query_block_size`, `query_workers`, `max_temporary_bytes`, `max_snapshot_bytes`, `snapshot_ttl`, `query_timeout` | Positive budgets; defaults 512, 2, 1 GiB, 256 MiB, 10 minutes, and 30 seconds. | Same named `library.Config` fields. Exhaustion returns a typed failure, never a short page. |
| `bm25_k1`, `bm25_b`, `rrf_k` | Finite numbers; defaults 1.2, 0.75, and 60. | Same named `library.Config` fields. Query settings enter the cursor identity. |

The global `conversation.include_subagent_conversations` setting currently controls raw index visibility. C4 must make semantic subagent admission independent of ordinary list visibility while continuing to use the registered provider readers. Unknown selectors, missing enabled store fields, invalid numeric budgets, and model descriptor mismatches fail configuration or opening before search accepts requests. The old `socket_path` key is rejected with a migration error after the RPC client is removed.

## Error contract

| Library result | Clyde CLI and MCP result | gRPC status |
| --- | --- | --- |
| `ErrInvalidRequest`, `ErrCursorMismatch` | `conversation_search_source_refused` with the safe reason. | `InvalidArgument` |
| `ErrCursorExpired` | `conversation_search_source_refused` with a restart paging reason. | `FailedPrecondition` |
| `ErrStoreMismatch` | Opening the mismatched store fails. Any prior valid generation remains readable. | `FailedPrecondition` |
| `ErrAppendConflict`, `ErrStaleGeneration` | The ingestion operation fails. Freshness reports pending work or the error; search continues on prior committed data. | `FailedPrecondition` |
| `ErrVectorMissing`, `ErrVectorCorrupt` | `conversation_search_source_failed`; never return a partial page. | `Internal` |
| `ErrDeadline` | `conversation_search_source_failed` with a deadline reason. | `DeadlineExceeded` |
| `ErrResourceLimit` | `conversation_search_source_refused` with a resource reason. | `ResourceExhausted` |
| Disabled search switch | Existing `conversation_search_disabled`. | `FailedPrecondition` |

Unexpected library errors become `conversation_search_source_failed` and `Internal`. Clyde logs the cause without transcript text. An offset request consumes a cursor chain inside one library snapshot; the public result retains `offset`, `next_offset`, and `has_more`. A later independent offset request opens a new snapshot and can see newly committed rows. C3 adds optional request `cursor`, response `next_cursor`, and per-match `context_state` fields to the daemon protobuf, domain types, CLI JSON, and MCP result. The CLI accepts `--cursor`; the MCP operation accepts the same optional value. Old clients can ignore the new fields. A cursor continues under the same query, filter, ranking settings, and snapshot; expiry or mismatch returns the typed error above.

## Tasks

### C1. Prepare immutable conversation occurrences

Depends on: LMS L0 public facade and build contract.

Files:

- Modify: [internal/daemon/conversation_semantic_documents.go](../../../internal/daemon/conversation_semantic_documents.go).
- Modify: [internal/daemon/conversation_semantic_content_policy.go](../../../internal/daemon/conversation_semantic_content_policy.go).
- Create: `internal/conversation/searchbackend/types.go`.
- Create: `internal/conversation/searchbackend/rows.go`.
- Create: `internal/conversation/searchbackend/filter.go`.
- Create: `test/live/embedded_conversation_search_test.go` for real provider artifacts, a real daemon, and the public CLI and MCP search operations.

Behavior:

- Keep `BuildSemanticConversationDocuments` as the sole transcript projection. Produce selected nonempty chat, tool, output, and thinking rows according to typed content settings. Keep original message positions even when earlier messages are omitted.
- Pass each selected logical row to the library's `PrepareText` helper. Use its ordered parts, stable suffixes, exact embedding input, and source spans without splitting again in Clyde or `Apply`. Keep whole-field identity in Clyde's projection state. Each immutable library row key includes stable provider message identity, content kind, tool index when applicable, part suffix, and explicit projection profile. Routine batch generation does not change row identity. Supply namespace, owner ID, row key, sort key, selected text, embedding input, source span, original message index, source identity, `loadRules`, and typed scalar metadata through the exported API. Store selected excerpt text but never store excluded fields for later context.
- Move tool content construction from LMS into Clyde. Preserve tool name, display, bash shell tokens, opaque fallback, read and write targets, and token deduplication. Preserve existing UTF-8 handling and blank field exclusion.
- Build generic typed filters from Clyde search options. Keep provider, workspace, archive, subagent, role, time, and selected projection profile policy in Clyde. Use generic `Prefix` for workspace matching and `In` for explicit conversation IDs. The library sees declared scalar columns and filter operators only.

Steps:

1. Add the occurrence and filter types after L0 fixes the library's exported names and error contract. Preserve `SemanticProjectionHash`'s existing byte encoding for any old checkpoint comparisons. Derive stable row identity independently of the routine batch generation.
2. Run a one-time direct old and new converter comparison on a bounded real sample before removing the converter. Record row and token parity as migration evidence without committing a copied output fixture.
3. Count selected and omitted content on a bounded real Claude, Codex, Cursor, and Zed sample without printing transcript text. Confirm excluded providers, roles, and content classes send no embedding input.

Verification:

- Run: `go test -tags live ./test/live -run '^TestEmbeddedConversationSearchContentSelection$' -count=1` in the supported pinned source workspace.
- Expect: The public CLI and MCP search find selected chat, tool names, bash programs, and file targets in real temporary provider transcripts. They exclude unselected output and thinking. A tool-only message retains its original index. The temporary Milvus and SQLite stores receive only selected nonempty content.

### C2. Commit append only ingestion and recover it

Depends on: C1 and LMS L1 catalog and append semantics.

Files:

- Modify: [internal/daemon/conversation_semantic_sync.go](../../../internal/daemon/conversation_semantic_sync.go).
- Create: `internal/daemon/conversation_semantic_outbox.go`.
- Extend: `test/live/embedded_conversation_search_test.go` with restart and replay cases.
- Modify: [internal/cli/daemon/backfill.go](../../../internal/cli/daemon/backfill.go) for the new explicit operator behavior.

Behavior:

- Keep the existing provider index, growing artifact deferral, bounded admission, failed load reporting, and freshness accounting. Replace manifest needed-set and full owner replacement with a durable outbox of immutable occurrence batches. Store batch ID, source identity, policy version, occurrence IDs, selected row counts, and library receipt.
- Write the outbox before append. Stage only new prepared parts for one owner generation. At EOF, call `CommitGeneration` with `GenerationSeal{RowCount, ManifestHash}` and mark delivery complete only after its receipt identifies the committed uint64 generation. Replay an unacknowledged batch after restart. Replayed stable row keys must have byte-identical immutable fields. The library publishes all verified staged parts atomically. Appended messages create only their new occurrences; a truncated or lost source preserves old rows. Metadata-only changes use `ReprojectScalars` with exact row keys and perform zero vector writes.
- Distinguish failed source reads from an empty selected projection. Suppress repeated unchanged failures without claiming the source was indexed. Report source reading, projection, embedding, metadata persistence, and searchable completion separately.
- Call `ReprojectScalars` for previously indexed owners when archive, workspace, provider, or subagent classification changes. Supply a monotonic `ProjectionOrder`, an idempotency token, and mutable declared values keyed by exact stable row keys. Do this even when the current content admission policy excludes that owner. The operation writes no vectors and does not load excluded transcript fields. An absent source retains its last accepted metadata.
- Make old delete and scalar backfill commands explicit about their new semantics. Never translate a routine sync or backfill into automatic occurrence deletion. Leave destructive maintenance unavailable until a separately reviewed operator contract exists.

Steps:

1. Add a Clyde owned SQLite outbox under the configured state directory with versioned schema and WAL. Transactionally record intent before calling the library. Keep it distinct from the MITM capture database and the library's occurrence catalog.
2. Adapt the sync worker to C1's occurrence preparation and the library append facade. Keep its rotating batch cursor and retry pacing. Do not treat a missing raw record as a delete signal.
3. Stop and restart the real daemon at crash points before append, after library commit, and before receipt acknowledgment. Verify replay, complete part publication, and retained earlier messages through public CLI and MCP search.

Verification:

- Run: `go test -tags live ./test/live -run '^TestEmbeddedConversationSearchRecovery$' -count=1` in the supported pinned source workspace.
- Expect: An unchanged second pass sends zero occurrence rows and performs zero embeddings and zero vector writes. A restart replays only unacknowledged work. Source loss preserves the last accepted occurrence. No routine path deletes occurrences.

### C3. Prepare eligibility and return complete search pages

Depends on: C1 for filter meaning and LMS L3 for complete ranked paging. Filter preparation can begin after L0.

Files:

- Modify: [internal/daemon/conversation_search_source.go](../../../internal/daemon/conversation_search_source.go).
- Modify: [internal/daemon/search_engine_hits.go](../../../internal/daemon/search_engine_hits.go).
- Modify: [internal/daemon/search_paging.go](../../../internal/daemon/search_paging.go).
- Create: `internal/daemon/conversation_embedded_search.go`.
- Extend: `test/live/embedded_conversation_search_test.go` with filter and paging cases.
- Modify: [internal/conversation/list.go](../../../internal/conversation/list.go) for optional request `Cursor`, result `NextCursor`, and per-match `ContextState`.
- Modify: [api/clyde/v1/daemon/service.proto](../../../api/clyde/v1/daemon/service.proto) and [internal/daemon/client.go](../../../internal/daemon/client.go) for additive wire fields.
- Modify: [internal/clispec/conversation_results.go](../../../internal/clispec/conversation_results.go) for shared CLI and MCP JSON results.
- Modify: [internal/clispec/conversationops.go](../../../internal/clispec/conversationops.go) for text rendering and cursor input.

Behavior:

- Translate Clyde policy into generic typed predicates before ranking. The library evaluates them against committed occurrence metadata within one query snapshot. Workspace matching keeps prefix semantics through generic `Prefix`. Explicit conversation IDs use generic `In`. Raw index updates can enrich future occurrence metadata; absence from a raw scan never excludes a stored occurrence. A known empty explicit ID set ends the request without a library call.
- Pass the generic typed filter, score floor, group key, per group cap, page size, and cursor to the library. The library evaluates committed occurrence metadata within one query snapshot. Consume exact pages to preserve current offset requests. Remove the three overfetch attempts, 2,000-row cap, and hidden or archived filtering after retrieval.
- Hydrate hit identity, selected excerpt, record metadata, message index, score, and `loadRules` from immutable occurrence data. A live context read checks the source stamp before and after loading and compares the matched message identity and relevant context slice with the stored occurrence. A later transcript append does not invalidate an unchanged earlier message. If the source is incompatible or missing, return the excerpt with explicit unavailable context. A malformed or ineligible hit is a typed source error, never a silently dropped row.
- Preserve existing facets, freshness, filter accounting, CLI and MCP fields, and gRPC error mapping. A failed library query remains a failure with no partial page.
- Apply the error contract table to unavailable, refused, deadline, resource, and failed requests. Keep `conversation_search_disabled` for the disabled switch. Log the underlying cause without exposing selected transcript text. Never return an empty successful page for a library error.

Steps:

1. Add a bounded public test corpus with many high-ranking excluded occurrences, more than one page, repeated content, an appended transcript, and a missing artifact. Test the production daemon search entry point rather than a mocked ranker.
2. Implement the eligibility builder from C1 filters. The library evaluates filters and hydrates hits from one committed query snapshot. Keep retained earlier messages searchable after later appends.
3. Integrate the library cursor. Verify exact `has_more`, stable order across page sizes, no duplicate or skipped occurrences, and equal CLI and MCP results.

Verification:

- Run: `go test -tags live ./test/live -run '^TestEmbeddedConversationSearchPaging$' -count=1` in the supported pinned source workspace.
- Expect: Every returned page is complete until the eligible set ends. A visible match below excluded matches appears. An appended artifact preserves correct context for unchanged earlier messages. An incompatible or missing artifact returns explicit unavailable context. Search errors remain typed errors.

### C4. Connect configuration, build, and daemon lifecycle

Depends on: C2 and C3 integration, plus LMS L0 native packaging gate.

Files:

- Modify: [internal/config/conversation_config.go](../../../internal/config/conversation_config.go).
- Modify: [internal/config/load.go](../../../internal/config/load.go) for typed validation and [internal/config/change_class.go](../../../internal/config/change_class.go) for `RouteReload` classification.
- Modify: [internal/daemon/conversation_semantic_runtime.go](../../../internal/daemon/conversation_semantic_runtime.go).
- Modify: [internal/daemon/run.go](../../../internal/daemon/run.go).
- Modify: [internal/daemon/control_server.go](../../../internal/daemon/control_server.go).
- Modify: [internal/daemon/runtime_status.go](../../../internal/daemon/runtime_status.go) and [internal/daemon/client_status.go](../../../internal/daemon/client_status.go).
- Modify: [internal/conversation/semsearch/client.go](../../../internal/conversation/semsearch/client.go) for complete removal after callers migrate.
- Modify: [go.mod](../../../go.mod), [go.sum](../../../go.sum), and platform build packaging.
- Modify: [docs/conversations.md](../../../docs/conversations.md) after the runtime behavior passes tests.

Behavior:

- Implement the keys, types, and defaults in the proposed configuration table. Preserve independent ingestion and search switches. Existing default content remains chat plus tool calls; archived, subagent, tool output, and thinking content remain excluded. Resolve the global `conversation.include_subagent_conversations` raw index setting with semantic selection so an enabled semantic setting can select subagent sources without making them visible in ordinary list results.
- Create and own the Milvus SDK client in Clyde. Inject `library/milvus` as the vector adapter and open the library once per daemon generation. The library also retains an offline `library/embedded` vector adapter for code search; Clyde's Milvus choice does not remove that profile. Stop ingestion before closing the library, adapter, and Milvus client during reload and shutdown. Recover after a transient unavailable store without selecting a different backend. Report connection and indexing status without an LMS socket or daemon process.
- Bind each catalog and vector pool generation to one resolved model, revision, dimension, and normalization descriptor. Treat `ErrStoreMismatch` as a configuration failure. A model setting change creates a new configured generation, builds it without modifying the prior one, and publishes it only after searchable completion. Preserve the prior generation for reads and recovery. Do not delete either generation automatically.
- Remove all conversation search and ingest RPC calls from Clyde after C2 and C3 pass. Keep CLI and MCP entry points. Coordinate LMS L5 removal only after supported installed Clyde clients use the new path.

Steps:

1. Pin the exact LMS library commit. Add `gksyntax` for Clyde's new shell projection and provision its grammar from a pinned source checkout in the supported build and release workspace until those sources ship in the module archive. Verify CGO and native linking on supported macOS and Linux builds. Run `GOWORK=off` only as an optional packaging gate after a self-contained module build becomes available.
2. Add typed configuration and validation. Reject unknown selectors and impossible dimensions before daemon startup. Keep the current default policy and independent read and write switches.
3. Wire the library and Milvus client into the daemon lifecycle. Use the existing worker phase ordering. Update status and source error mapping. Remove unused gRPC client code only after C2 and C3 compile and pass.
4. Run a real temporary Milvus and SQLite acceptance path with LMS daemon absent. Exercise the same query through CLI and MCP, then restart and repeat it. Keep production unchanged.

Verification:

- Run: `make test` and `make check` in the supported pinned source workspace.
- Run: `go test -tags live ./test/live -run '^TestEmbeddedConversationSearch' -count=1` in the supported pinned source workspace with a real Milvus client, temporary SQLite stores, and no LMS daemon.
- Expect: CLI and MCP return the same complete ordered hits. Search-only and ingestion-only modes work independently. Restart preserves committed search. A failed Milvus or store operation returns a typed error. Native builds pass on supported platforms.

## Measure before release

Use a fixed representative query set and a full corpus count. Record source conversation and message counts, selected nonempty rows, embedded distinct content, reused vectors, occurrences, vector and SQLite bytes, model startup, clean build time by stage, restart time, largest single artifact load, peak and steady memory, and filtered and unfiltered latency distributions. Compare search coverage and latency with the deployed path using the same eligible corpus and queries. Report any quality differences. A benchmark that times out or returns a short page fails acceptance. Do not infer full corpus duration from historical Milvus row counts; estimate it from Clyde's selected content and label the estimate. Run the test on actual 16 GB and 24 GB configurations or identify the representative environment precisely. Production release and any one-time reset remain separate decisions.
