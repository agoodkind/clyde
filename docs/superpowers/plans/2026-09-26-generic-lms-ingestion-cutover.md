# Clyde generic LMS ingestion cutover implementation plan

## Goal

Complete CLYDE-629. Clyde registers its collection, syncs fingerprints, streams conversation rows, backfills scalars, and deletes items through the generic LMS RPCs. Clyde determines conversation content and scalar values before it calls LMS. An unchanged corpus causes no re-offer or row migration.

## Current behavior

`internal/daemon/conversation_semantic_documents.go` calls `BuildSemanticConversationDocuments` after Clyde's existing conversation index and content policy select messages. `internal/conversation/semsearch/client.go` sends `ConversationDocument` objects to LMS, where conversation-specific code generates text, tool, and thinking rows and derives `provider` from the conversation ID. `internal/daemon/conversation_semantic_sync.go` sends fingerprints and the needed document set. `internal/cli/daemon/backfill.go` calls the conversation-specific scalar and document backfill RPCs. `internal/daemon/conversation_semantic_runtime.go` registers the collection. The sync worker recognizes a conflicting active job by matching error text.

## Constraints

- Requires deployed LMS-15, LMS-16, and LMS-17 with their old-RPC parity gates passed. Keep Clyde's raw provider readers, conversation index, `BuildSemanticConversationDocuments`, content policy, load rules, manifest construction, needed-set scheduling, suppression, and operator batch limits. Do not add another ingestion pipeline.
- Preserve each existing logical row key, physical split-part key, stored text, tool shell tokens, thinking content, scalar, and fingerprint. The old LMS conversion in `internal/daemon/manager_conversation_text.go`, `manager_conversation_tools.go`, and `manager_conversations.go` is the parity reference. LMS keeps the existing physical text splitting and part suffix generation.
- Keep the existing `conversationId`, `parentConversationId`, `role`, `provider`, `workspaceRoot`, `archived`, `timestampUnix`, `messageIndex`, and `loadRules` schema. Set `item_id_column=conversationId`. Clyde sends `provider` explicitly.
- Default every normal and operator upsert to retain. Keep `backfill_delivered` for reexamination and reserve `force_reexamine` for the existing explicit rebuild behavior. A transport or schema error never selects another backend automatically.
- Pin a released LMS commit in `go.mod`; do not add a machine-specific `replace`. Verify with `GOWORK=off`.
- Apply the configured provider, role, and content selections in Clyde before any LMS call. Send only nonempty selected content that needs embedding. Omit a message only when every selected content class is empty. Keep a message with empty text when it contains selected tool or reasoning content, and send those rows.
- Keep `SemanticProjectionHash` bytes and LMS checkpoint values compatible. A second pass over an unchanged corpus sends no row for embedding.

## Pull request boundary

Implement Tasks 1 through 3 and the pre-merge parity test from Task 4 in one CLYDE-629 pull request. The shared row projector and generic LMS delivery must pass together; a separate unused projector would not complete a user-visible operation. Deploy the cutover and inspect the normal feeder after that pull request passes its checks.

## Tasks

### 1. Implement Clyde's row projection and schema declaration

Files:

- Modify: `internal/conversation/semsearch/client.go`
- Modify: `internal/daemon/conversation_semantic_documents.go`
- Create: `internal/conversation/searchbackend/types.go`
- Create: `internal/conversation/searchbackend/rows.go`
- Create: `internal/conversation/searchbackend/rows_test.go`
- Create: `internal/conversation/searchbackend/testdata/legacy_rows.json` from synthetic provider fixtures processed through the old LMS converter before cutover.
- Create: `internal/daemon/generic_lms_rows_corpus_test.go` for the opt-in existing-reader corpus check.
- Modify: `go.mod`
- Modify: `go.sum`

Behavior:

- Define one typed conversation schema declaration for registration and one typed row projection from the existing `SemDoc`. The projection creates text, tool, and thinking logical rows with exact existing base keys. It preserves the current UTF-8 cleanup, blank-content exclusion, tool display and shell-token construction, duplicate-token removal, and row scalar values. LMS splits long row text and generates the same physical part suffixes as before.
- Derive `provider` from Clyde's typed conversation identity, then send it as a declared scalar. Set `item_id` and `conversationId` to the same conversation ID. Preserve parent ID, role, timestamp, message index, workspace, archived, and `loadRules` per row.
- Send the exact existing fingerprint values. The generic protocol consumes Clyde's manifest as provided; row projection does not recalculate fingerprints.

Steps:

1. Pin the LMS commit that contains all three generic ingestion RPC units. Run `GOWORK=off go mod tidy` and inspect `go.mod` and `go.sum`.
2. Define backend-neutral document and tool types in `internal/conversation/searchbackend/types.go`. Keep `BuildSemanticConversationDocuments` as the sole policy and transcript-loading path. Move its current fields into those types and use temporary `semsearch.SemDoc` and tool aliases if existing callers need them. Preserve `SemanticProjectionHash`'s exact field order and length-prefixed encoding. Unchanged conversations retain their LMS fingerprints. Derive logical rows once in `searchbackend/rows.go` and send them to the generic LMS adapter. Use a separate digest for local passage invalidation.
3. Before removing the old converter, capture its logical rows for synthetic provider fixtures in `legacy_rows.json`. Add table cases for plain text, long UTF-8 text, empty content, tools with shell decomposition, duplicate tokens, thinking, forks, archived records, and every provider. Compare row keys, content, and scalars with that baseline.
4. Add an opt-in full-local-corpus projection audit that reads each transcript through Clyde's existing readers and runs `BuildSemanticConversationDocuments` and the shared row projector. Report conversation and row counts, unstable keys, projection errors, and blank-only rows without printing transcript text. The isolated LMS ingest parity test checks physical split keys and vectors on a bounded real sample.

Verification:

- Run: `GOWORK=off go test ./internal/conversation/semsearch ./internal/daemon`
- Expect: row fixtures and existing document policy tests pass without a local module replacement.
- Run: `CLYDE_SEMANTIC_CORPUS_PARITY=1 GOWORK=off go test ./internal/daemon -run TestGenericLMSRowsMatchLocalCorpus -count=1`
- Expect: every locally indexed Claude, Codex, Cursor, and Zed conversation projects without an unstable key or conversion error. The synthetic fixtures and isolated LMS test provide old/new row parity; record full-corpus counts without transcript text.

### 2. Cut registration, manifest sync, and upsert stream over together

Files:

- Modify: `internal/conversation/semsearch/client.go`
- Modify: `internal/daemon/conversation_semantic_runtime.go`
- Modify: `internal/daemon/conversation_semantic_sync.go`
- Modify: `internal/conversation/semsearch/client_test.go`
- Modify: `internal/daemon/conversation_semantic_sync_test.go`

Behavior:

- `Client.Register` calls `RegisterCollection` with the exact declaration. `SyncConversationManifest` calls `SyncCollectionManifest` and returns the same needed IDs. `UpsertConversationDocuments` and `ReexamineConversationDocuments` send bounded generic row frames, a full manifest, retain mode, and the appropriate backfill flag. Keep the existing per-frame byte and count checks.
- The daemon sync worker keeps its needed set, delivery cursor, suppression rules, active-job pacing, and freshness counts. A typed LMS `ErrorInfo` reason identifies an active-job conflict; Clyde no longer checks error text. Registration rejects a typed schema mismatch and logs the conflicting column.
- The cutover changes only the LMS transport at the existing client boundary. No config switch silently selects old RPCs or another search backend after an error.

Steps:

1. Replace the old RPC calls in `client.go` while retaining its conversation-level methods for daemon callers. Convert the existing fingerprints and `SemDoc` values at this boundary.
2. Decode stable `ErrorInfo` reason and metadata in the client, then replace `isConflictingActiveJob` text matching in the worker. Keep all other errors visible as failures.
3. Exercise the production sync worker through its public daemon boundary against a sandbox LMS daemon and temporary collection. Verify the first pass queues a job, a repeated unchanged pass queues none, and a concurrent upsert keeps the existing coalescing behavior.

Verification:

- Run: `GOWORK=off go test ./internal/conversation/semsearch ./internal/daemon`
- Expect: register, needed-ID scheduling, bounded stream, coalescing, suppression, and unchanged-pass tests pass.

### 3. Cut operator maintenance over

Files:

- Modify: `internal/conversation/semsearch/client.go`
- Modify: `internal/cli/daemon/backfill.go`
- Modify: `internal/cli/daemon/backfill_test.go`

Behavior:

- `BackfillConversationScalars` streams generic item IDs with `workspaceRoot` and `archived` values. Dry run remains the CLI default and reports the existing changed and orphan counts. `DeleteConversation` uses `DeleteCollectionItem`. Document reexamination uses the generic upsert with `backfill_delivered=true`.
- Keep the existing `--conversation`, `--limit`, `--after`, `--all`, and `--execute` selection and safety behavior. Do not modify provider-owned artifacts.

Steps:

1. Replace maintenance RPC calls in `client.go`; keep the CLI commands and output stable.
2. Test the CLI through its command boundary against a temporary LMS collection. Compare dry-run counts with the old adapter, execute a bounded backfill, and inspect vectors and unchanged checkpoints.

Verification:

- Run: `GOWORK=off go test ./internal/cli/daemon ./internal/conversation/semsearch`
- Expect: dry run writes nothing; executed backfill changes only eligible scalars; document reexamination preserves its existing batch bounds.

### 4. Prove the cutover and deploy

Files:

- Modify: `docs/conversations.md`

Behavior:

- A sandbox ingest through the new RPCs stores rows equal to the old LMS conversation path. After deployment, the existing production collection remains readable. The normal feeder reports no needed items for unchanged fingerprints, and no automatic full re-embed starts.

Steps:

1. Run old and generic ingest against isolated temporary LMS collections using the same bounded real transcript sample. Compare row keys, content, scalar values, vectors, and checkpoint fingerprints. Keep transcript text out of logs and test output.
2. Check the Clyde-to-LMS boundary on that sample with actual counts and payload inspection. Count conversations, messages, selected rows, rows sent, and rows embedded. Confirm that every sent row is nonempty selected content, that excluded providers, roles, and content classes send nothing, and that tool-only and reasoning-only messages with empty text send their rows. Run a second unchanged pass and confirm that it embeds zero rows and leaves `SemanticProjectionHash` values and checkpoints unchanged. Report metadata reconciliation (manifest sync, fingerprint and checkpoint updates, scalar work) separately from embedding work in both counts and time.
3. Measure a clean install of the sample from a fresh Clyde and LMS state. Record elapsed time for source reading, projection, embedding, persistence, and searchable completion; the embedding model startup time; embedding throughput; and total elapsed time. Estimate the full corpus from eligible content counts after Clyde's selections, not from historical Milvus row counts, and label the result as an estimate.
4. Update the existing conversation documentation to state that Clyde creates search rows and LMS ingests declared items.
5. Run `make test` and `make check`. Build, install, and reload Clyde through the repository's supported daemon procedure, then inspect a complete normal feeder pass and a fresh ingest of one changed conversation.

Verification:

- Run: `GOWORK=off make test && GOWORK=off make check`
- Expect: all tests and lint pass without `go.work`.
- Expect: the boundary check in step 2 reports only nonempty selected rows sent, the expected tool-only and reasoning-only rows, and zero rows embedded on the unchanged second pass.
- Expect: the clean-install measurement in step 3 reports each stage time, model startup time, throughput, sample counts, and a labeled full-corpus estimate.
- Expect: deployed LMS accepts the generic calls; the unchanged corpus produces zero re-offered items; one changed conversation updates only its own rows and checkpoint.
