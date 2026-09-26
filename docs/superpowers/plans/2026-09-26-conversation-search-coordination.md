# Conversation search coordination implementation plan

## Goal

Release generic LMS collection ingestion and search, move Clyde to those RPCs, add Clyde's local in-memory search backend, measure it on the full conversation corpus, and remove the retired LMS conversation protocol. Clyde uses one provider-artifact ingestion path and one prepared search contract for both backends. `backend = "lms"` returns an unavailable error when LMS is down. `backend = "local"` searches without LMS, Docker, or a model provider.

## Current behavior

LMS currently exposes conversation-specific RPCs. Clyde's LMS client sends conversation documents and filters through those RPCs. Clyde's existing raw conversation index and `BuildSemanticConversationDocuments` already select provider content and create the message sequence. The [local search plan](2026-09-26-local-conversation-search.md) defines the bundled model, compact persisted index, RAM search, and measurements. The LMS and Clyde generic protocol plans define separate cutovers and compatibility gates.

## Constraints

- Implement shared Clyde row preparation once. The [Clyde ingestion cutover](2026-09-26-generic-lms-ingestion-cutover.md) and local search Task 2 use the same change for CLYDE-629 and CLYDE-752.
- Implement shared Clyde filter preparation once. The [Clyde search cutover](2026-09-26-generic-lms-search-cutover.md) and local search Task 3 use the same change for CLYDE-643 and CLYDE-752.
- Deploy LMS generic RPCs while Clyde still calls the old RPCs. Keep the old RPCs until both Clyde cutovers are deployed and supported installed Clyde versions use the generic client.
- Preserve the existing ingestion worker, provider readers, content policy, conversation IDs, row keys, `SemanticProjectionHash` bytes, stored LMS rows, and checkpoint values. The local backend uses the same prepared rows and a separate local passage digest. It builds its index from raw conversations.
- Keep `ingestion_enabled` and `search_enabled` independent. Require an explicit backend when either switch is enabled. Never change the configured backend after a failure or an empty result.
- Treat tests, a build, deployment, and live acceptance as separate gates. Record each gate before starting a dependent release.

## Execution order

| Order | Work | Depends on | Required result |
| --- | --- | --- | --- |
| 1 | LMS-15 registration from the [LMS ingestion plan](https://github.com/agoodkind/lm-semantic-search/blob/d9f502d8312cb9b2b6e3e83df6d95d7251e724ef/docs/superpowers/plans/2026-09-26-generic-collection-ingestion.md). | Existing LMS. | Clyde remains on old RPCs. Registration validates saved declarations in both profiles and existing Milvus schemas when present. It changes no checkpoint or feeder needed set. |
| 2 | LMS-16 manifest and upsert from the LMS ingestion plan. | Order 1. | Old and generic requests write equal rows and fingerprints. Clyde's unchanged feeder reports zero re-offer. |
| 3 | LMS-17 backfill and delete from the LMS ingestion plan. | Order 2. | Old and generic dry runs report equal counts. A normal Clyde feeder pass succeeds. |
| 4 | LMS-18 typed search from the [LMS search plan](https://github.com/agoodkind/lm-semantic-search/blob/d9f502d8312cb9b2b6e3e83df6d95d7251e724ef/docs/superpowers/plans/2026-09-26-generic-collection-search.md). | Order 1. | Old and generic searches return equal ordered hits, scores, and fingerprints. Clyde still calls the old RPCs. |
| 5 | CLYDE-629 and local search Task 2. | Orders 1 through 3. | Clyde prepares rows once and uses the generic LMS ingestion RPCs. An unchanged corpus causes no re-offer. |
| 6 | CLYDE-643 and local search Task 3. | Orders 4 and 5. | Clyde prepares filters once and uses generic LMS search. Live CLI and MCP results, freshness, and context windows remain equal. |
| 7 | Local search Task 4, CLYDE-753. | Order 5. | A bundled model runs offline and splits long eligible rows into searchable passages. |
| 8 | Local search Task 1, CLYDE-751. | Order 6. | Config selects exactly one backend; enabled sandbox fixtures declare LMS explicitly. |
| 9 | Local search Task 5, CLYDE-754. | Orders 5, 7, and 8. | Clyde persists a compact index, loads search data into RAM, and refreshes it from the existing ingestion worker. |
| 10 | Local search Task 6, CLYDE-755. | Orders 6 and 9. | Local CLI and MCP search work with LMS absent; selected LMS outage returns an error. |
| 11 | Local search Task 7, CLYDE-756, then local-mode release. | Order 10. | Full-corpus measurements establish model and index size, build and startup cost, peak and steady RAM, and query latency on 16 GB and 24 GB configurations. |
| 12 | [LMS protocol retirement](https://github.com/agoodkind/lm-semantic-search/blob/d9f502d8312cb9b2b6e3e83df6d95d7251e724ef/docs/superpowers/plans/2026-09-26-generic-collection-retirement.md). | Orders 5 and 6 deployed; supported installed Clyde versions use generic RPCs. | Seven old RPCs and obsolete conversion code are removed. Existing collections, vectors, scalars, and checkpoints remain readable. |

LMS-18 implementation may proceed after order 1 while LMS-16 and LMS-17 are in progress. Integrate LMS changes against the latest `service.proto` and regenerate protobuf code after each merge. Local model work may proceed after order 5 while order 6 is in progress. Integrate Clyde changes to `internal/conversation/semsearch/client.go` serially.

## Tasks

### 1. Release the LMS protocol beside the old protocol

Files:

- Execute the LMS ingestion plan, tasks 1 through 4.
- Execute the LMS search plan, tasks 1 through 3.

Behavior:

- Release LMS-15, then LMS-16, then LMS-17. LMS-18 depends only on LMS-15 and may be developed concurrently, but its release must pass the live old/new search battery before the Clyde search cutover.
- Leave Clyde on the old RPCs for each LMS deployment. A registration or protocol addition must not alter existing rows, checkpoint fingerprints, or the unchanged feeder's needed set.

Steps:

1. Complete each LMS task's public gRPC and real-store tests before deploying that unit.
2. Deploy each LMS unit with Clyde unchanged. Inspect a normal feeder pass and run that unit's isolated live parity gate.
3. Record the LMS commit and live gate result before starting its dependent Clyde cutover.

Verification:

- Run in LMS: `make proto && make test && make check`.
- Run in LMS after LMS-16: `go test -tags live -run '^TestGenericCollectionIngestParity$' -count=1 ./test/live/`.
- Run in LMS after LMS-18: `go test -tags live -run '^TestGenericCollectionSearchParity$' -count=1 ./test/live/`.
- Expect: old and generic paths match; the unchanged Clyde feeder reports zero needed items and no new embed job.

### 2. Cut Clyde's LMS ingestion and search over using shared contracts

Files:

- Execute the [Clyde ingestion cutover](2026-09-26-generic-lms-ingestion-cutover.md) together with local search Task 2.
- Execute the [Clyde search cutover](2026-09-26-generic-lms-search-cutover.md) together with local search Task 3.

Behavior:

- CLYDE-629 creates Clyde-owned logical rows and sends them through generic LMS ingestion. CLYDE-643 creates the typed filter before backend dispatch and sends it through generic LMS search. The local backend consumes these same contracts in later tasks.
- Keep the conversation-level CLI and MCP contracts. Keep selected LMS errors visible and do not use the local backend as a fallback.

Steps:

1. Complete CLYDE-629 after LMS-15 through LMS-17 pass deployed parity. Compare shared rows with old LMS output from synthetic fixtures and a bounded isolated real sample. Run the full-local-corpus projection audit for counts, stable keys, and errors. Pin the LMS dependency commit without a local `replace`. Deploy and inspect the normal feeder pass.
2. Complete CLYDE-643 after LMS-18 passes deployed parity. Run old/new search comparisons on the live collection. Deploy and check cross-provider and conversation-scoped search, indexed item-state compatibility, and a `--around` read using the hit's `loadRules`.
3. Mark local search Tasks 2 and 3 complete from these shared implementations. Do not create a second row projector or filter builder.

Verification:

- Run in Clyde: `CLYDE_SEMANTIC_CORPUS_PARITY=1 GOWORK=off go test ./internal/daemon -run TestGenericLMSRowsMatchLocalCorpus -count=1`.
- Run in Clyde: `GOWORK=off make test && GOWORK=off make check`.
- Run in Clyde after CLYDE-643: `GOWORK=off go test -tags live -run '^TestGenericLMSSearchLiveParity$' -count=1 ./internal/daemon/`.
- Expect: unchanged fingerprints require no re-offer; old and generic search return equal ordered hits and context windows.

### 3. Build, integrate, and measure local search

Files:

- Execute the [local search plan](2026-09-26-local-conversation-search.md), tasks 4, 1, 5, 6, and 7 in that order.
- Use the [local search design](../specs/2026-09-22-local-conversation-search-design.md) for backend selection and index behavior.

Behavior:

- Task 4 bundles a small model and splits long rows. Task 1 adds explicit backend selection. Task 5 persists the index and refreshes it through Clyde's existing ingestion worker. Task 6 connects local retrieval to the existing CLI and MCP search path. Task 7 measures the full corpus before local mode is released.
- A complete prior local generation remains searchable during refresh. Local search loads packed vectors and scalar metadata into RAM. Full transcript text remains in provider artifacts.

Steps:

1. Complete and test the model and index while the generic LMS backend remains deployed.
2. Add backend selection and local runtime integration together. Set existing enabled configurations explicitly to `backend = "lms"` before applying the config change. Enable `backend = "local"` only when the local runtime is ready.
3. Run CLI and MCP tests in an isolated environment without LMS or Docker and with network access disabled for the bundled model. Run the selected-LMS outage test separately and verify that it returns the typed unavailable error.
4. Measure the complete Codex, Cursor, and Claude corpus on 16 GB and 24 GB configurations or report the exact representative environments. Record all metrics required by local search Task 7. Release local mode after those measurements confirm usable build and search behavior.

Verification:

- Run in Clyde: `go test ./internal/conversation/localsearch/... -count=1`.
- Run in Clyde: `go test ./internal/config ./internal/daemon -run 'Semantic|ConfigChange' -count=1`.
- Run in Clyde: `GOWORK=off make test && GOWORK=off make check`.
- Expect: local CLI and MCP search succeed without LMS, Docker, or a model provider; selected LMS outage returns an error; the measured index and RAM use support the stated machine configurations.

### 4. Remove the old LMS protocol

Files:

- Execute the LMS protocol retirement plan.

Behavior:

- Remove old RPC declarations, handlers, conversation-only conversion, obsolete tests, and documentation after both Clyde cutovers and installed-client compatibility are verified. Preserve old rows and their schema and checkpoint interpretation.

Steps:

1. Confirm deployed CLYDE-629 and CLYDE-643 use only generic RPCs. Confirm every supported installed Clyde version uses the generic protocol.
2. Execute the retirement tasks in their stated order. Pin the LMS removal commit in Clyde and run `GOWORK=off` gates before deploying LMS.
3. Deploy LMS and repeat live Clyde ingest, search, scalar backfill, and context-window checks against the existing collection. Compare separate read-only row and checkpoint observations before and after deployment. Run an unchanged manifest sync to confirm zero re-offer.

Verification:

- Run in LMS: `make proto && make test && make check`.
- Run in Clyde: `GOWORK=off make test && GOWORK=off make check`.
- Expect: generated LMS service excludes the seven old RPCs and Clyde uses generic RPCs. The separate live observations confirm unchanged existing rows and checkpoints.
