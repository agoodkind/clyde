# Local conversation search implementation plan

## Goal

`clyde conversation search --query "..."` and the `clyde_search` MCP tool search provider conversations with `backend = "local"`. The local backend bundles a small model, saves a compact index on disk, and loads search data into RAM. It requires no LMS process, Docker service, or model provider. `backend = "lms"` retains LMS behavior and returns an unavailable error when LMS is down. Both backends use Clyde's existing ingestion and search operations.

The [design specification](../specs/2026-09-22-local-conversation-search-design.md) defines the product behavior. Tack epic CLYDE-750 tracks CLYDE-751 through CLYDE-756.

## Current behavior

`ConversationSemanticConfig` has independent `ingestion_enabled` and `search_enabled` switches but no backend selector. `startConversationSemanticRuntime` dials LMS when either switch is true. `RunInitialConversationIndex` exits when the raw cache exists, before any semantic indexing.

`conversationSemanticSyncWorker` reads `conversation.Index`, builds manifests, schedules bounded batches, and calls `BuildSemanticConversationDocuments`. That projection omits messages with no selected content, preserves tool-only messages, and keeps original message indexes. Its output type and client calls currently depend on LMS. `semanticConversationSearchSource` is the only implementation of `conversationSearchSource`. `engineSearchFilter`, `resolveEngineHits`, and `semanticSearchResult` contain filter, visibility, hydration, and paging behavior needed by both backends.

## Constraints

- Read provider artifacts through the existing raw index and readers. Add no second parser, artifact scan, or ingestion worker. Never write provider-owned files.
- Apply content selection and derive text, tool, and thinking rows in Clyde before backend dispatch. The local backend may split eligible rows for its model.
- Require `local` or `lms` when ingestion or search is enabled. Keep the switches independent. Never select a backend based on availability or query results.
- Keep the current CLI and MCP search contracts. Clyde owns filter meaning, visibility, hydration, paging, facets, freshness, and typed errors. Backends execute prepared scalar restrictions during retrieval.
- Keep a prior complete local generation searchable during refresh. Load packed vectors and scalar metadata into RAM. Leave full transcript text in the raw artifacts.
- Treat the earlier 7.6 to 8.6 million passage count and 390 to 444 MiB of bare 384-bit vectors as estimates. They exclude the model, metadata, runtime, disk overhead, and temporary memory.
- CLYDE-629 depends on LMS-15, LMS-16, and LMS-17 for generic LMS ingestion. CLYDE-643 depends on LMS-18 for generic LMS search. Complete those Clyde cutovers before removing the current LMS-specific calls. Local work can implement Clyde-owned contracts without LMS at runtime.

## Pull request boundaries

Complete Task 2 in the CLYDE-629 ingestion pull request and Task 3 in the CLYDE-643 search pull request. Implement Task 4 as a separate offline model and passage pull request. Implement Task 5 storage and refresh internals as a separate pull request after the model; keep daemon backend selection unchanged in that intermediate release. Implement Task 1, the remaining Task 5 daemon wiring, Task 6, and Task 7 in the local activation pull request. That pull request must include working local CLI and MCP search, selected-LMS outage coverage, and full-corpus measurements before local mode is released.

## Tasks

### 1. Validate the backend setting (CLYDE-751)

Files:

- Modify: `internal/config/conversation_config.go`, `internal/config/load.go`, `internal/config/conversation_semantic_directions_test.go`, `internal/config/change_class_test.go`.
- Modify: `internal/daemon/conversation_semantic_runtime.go`, `internal/daemon/run.go`.
- Modify: `internal/cli/daemon/sandbox.go` and its tests. An enabled sandbox writes an explicit backend.
- Modify: `internal/daemon/initial_index_test.go`, `test/live/conversation_config.toml.tmpl`, `test/live/harness_semantic_config_test.go`.

Behavior:

- Add a typed `ConversationSemanticBackend` with only `local` and `lms` values. A disabled semantic section may omit `backend`. An enabled section with a missing or unknown backend fails config loading with `conversation.semantic.backend` in the error.
- Preserve independent ingestion and search settings. Dial and register LMS only for selected LMS. A backend edit uses `RouteReload`. The new daemon generation opens the selected runtime before serving.

Steps:

1. Add TOML and JSON tags for `backend`. Make the config defaulting and validation path return an error. Propagate the error through `applyLoggingDefaultsAndValidate`, `NewConfigWithDefaults`, and config loading. Update tests for disabled, ingestion-only, search-only, and both-enabled combinations. Add `backend = "lms"` to existing enabled sandbox and live fixtures.
2. Gate `startConversationSemanticRuntime` on selected LMS. Preserve its LMS retry behavior. Select the runtime by the typed enum in `run.go`; connect the local branch after Task 6.
3. Update existing enabled fixtures with an explicit backend. Before deploying the validation change, add `backend = "lms"` to each installed config that already enables semantic ingestion or search. Verify that an invalid enabled config fails before daemon startup. Verify that backend changes use the existing default `RouteReload` classification.

Verification:

- Run: `go test ./internal/config ./internal/daemon -run 'Semantic|ConfigChange' -count=1`.
- Expect: Enabled semantic config requires a valid backend. Disabled config needs none. Local selection makes no LMS dial. Selected LMS retains its retry and unavailable behavior.

### 2. Prepare one ingestion request in Clyde (CLYDE-752 and CLYDE-629)

Depends on: LMS-15, LMS-16, and LMS-17 for the LMS adapter. Clyde can define the shared row contract before Task 1 selects a backend.

Files:

- Create: `internal/conversation/searchbackend/types.go` for typed document, row, manifest, and ingestion contracts.
- Modify: `internal/daemon/conversation_semantic_documents.go`, `internal/daemon/conversation_semantic_sync.go`, `internal/daemon/conversation_semantic_content_policy.go`.
- Modify: `internal/conversation/semsearch/client.go` after the generic LMS ingestion protocol exists.
- Modify: `internal/daemon/conversation_semantic_blank_text_test.go`, `internal/daemon/conversation_semantic_sync_test.go`.

Behavior:

- `BuildSemanticConversationDocuments` remains the only message projection. It returns Clyde-owned typed documents. Clyde derives nonempty text, tool, and thinking rows before dispatch. Preserve conversation ID, parent ID, original message index, provider, workspace, role, timestamp, archive state, and `LoadRules`.
- Text uses `conv/<conversation-id>/<message-index>`. Each tool uses `convtool/<conversation-id>/<message-index>/<tool-index>`. Selected thinking uses `convthink/<conversation-id>/<message-index>`. Whitespace-only fields produce no row. Tool-only and selected thinking-only messages still produce rows.
- The existing sync worker retains manifest scheduling, 8 MiB artifact batch admission, growing-artifact deferral, empty-delivery memo, failed-load suppression, content-hash pinning, and freshness accounting. LMS and local adapters implement manifest, upsert, and job-state calls. A synchronous local upsert reports a completed job.

Steps:

1. Define typed `Document`, `Row`, `Fingerprint`, and `IngestClient` contracts with explicit scalar fields. Keep provider and LMS wire imports out of the contract package.
2. Change `BuildSemanticConversationDocuments` to return backend-neutral `Document` values. Keep `SemanticConversationLoadOptions` and `semanticToolCalls` as the only content selection and tool projection paths. Use aliases for existing `semsearch.SemDoc` and tool types if needed during cutover. Derive rows once per document.
3. Preserve `SemanticProjectionHash`'s existing field order and length-prefixed encoding exactly. The LMS manifest and stamp-only pinning depend on these bytes. Keep source artifact fingerprints separate. Compute a distinct local row-and-passage digest for local vector invalidation. A changed mtime with unchanged selected content needs no new vectors.
4. Adapt the sync worker at its client calls. Preserve its scheduling methods. Implement the LMS adapter against the generic ingestion RPC, including retain mode and row reconciliation. Keep model-specific passage splitting out of shared preparation.
5. Compare row keys, selected fields, omitted fields, and message indexes with CLYDE-629's golden corpus before changing production LMS delivery.

Verification:

- Run: `go test ./internal/conversation/... ./internal/daemon/... -run 'Semantic|Projection|Conversation' -count=1`.
- Expect: Clyde prepares one typed row set. Omitted messages do not renumber later indexes. Tool-only and selected thinking-only messages produce rows. LMS row keys and manifest behavior match the generic ingestion contract. Task 6 verifies the local adapter against the same projection.

### 3. Prepare one search request and result path (CLYDE-752 and CLYDE-643)

Depends on: Task 2. The LMS adapter requires LMS-18.

Files:

- Extend: `internal/conversation/searchbackend/types.go` with typed filter and hit contracts.
- Modify: `internal/daemon/conversation_search_source.go`, `internal/daemon/search_engine_hits.go`, `internal/daemon/search_paging.go`, `internal/daemon/search_conversations_engine_test.go`.
- Modify: `internal/conversation/semsearch/client.go` for the generic LMS search adapter.

Behavior:

- Clyde converts `SearchConversationsOptions` into a typed retrieval request. It resolves conversation and workspace scopes, validates bounds, and includes provider, role, time, archive, minimum score, and per-conversation restrictions. An empty workspace scope returns an empty result without querying a backend.
- Backends execute scalar restrictions while selecting candidates and return ranked hit IDs, message indexes, row kinds, passage positions, scores, and load rules. Clyde checks current raw records, deduplicates passages, applies visibility, offset, limits, facets, freshness, and typed errors. A backend error never becomes an empty success.

Steps:

1. Extract `engineSearchFilter` into Clyde's filter preparation. Preserve workspace prefix matching and the empty-scope short circuit.
2. Extract `resolveEngineHits` and `semanticSearchResult` into a common hit resolver. Deduplicate repeated passages before offset and per-conversation limits. Use a stable secondary key for equal scores. Keep bounded overfetch for hidden or stale hits.
3. Keep LMS RPC conversion and upstream error classification in its adapter. Preserve `conversationSearchSourceError` mapping at the public boundary.
4. Run CLYDE-643's side-by-side queries for provider, workspace, conversation, role, time, archive, score, offset, and per-conversation limit. Keep the existing CLI and MCP operation.

Verification:

- Run: `go test ./internal/daemon/... ./internal/conversation/... -run 'Search|Filter' -count=1`.
- Expect: LMS returns the existing public result shape and filter behavior, including `has_more` after bounded overfetch. An LMS query error retains its typed status.

### 4. Bundle the local model and split passages (CLYDE-753)

Depends on: Task 2. This work needs no LMS process.

Files:

- Create: `internal/conversation/localsearch/model.go`, `internal/conversation/localsearch/passage.go`, `internal/conversation/localsearch/assets/manifest.json`.
- Create: `internal/conversation/localsearch/assets/model.onnx` and `internal/conversation/localsearch/assets/tokenizer.json`. The model selection step must confirm that these assets work with the selected native runtime.
- Modify: `go.mod`, `go.sum` for the selected local runtime.
- Create: `docs/conversations/local-search-model.md` for the pinned model revision, license, digest, tokenizer, input limit, vector dimension, runtime, and asset size.

Behavior:

- The bundled tokenizer and model encode both rows and queries on the local CPU. Installation and search need no network access. A model or tokenizer change invalidates saved vectors.
- Split long eligible rows at token boundaries with overlap. Every source token appears in at least one passage. Each passage preserves the row key, original message index, content kind, tool index when present, passage ordinal, and source span. Do not truncate a row at the model limit.
- Select the smallest measured vector encoding that returns known relevant hits from a fixed Claude, Codex, and Cursor query sample. Compare one-bit vectors with a higher-precision encoding. Record the score calculation. The earlier 384-bit size estimate remains conditional until measurement.

Steps:

1. Validate redistribution terms, macOS and Linux support, tokenizer compatibility, and offline startup for the candidate model and runtime. Pin the exact asset digest before packaging.
2. Run a bounded sample through the candidate model and a model-free lexical baseline. Record aggregate rankings and asset sizes without committing transcript text. Choose the vector encoding from the same query sample.
3. Implement deterministic overlapping passage boundaries and bounded encoding batches. Register model runtime cleanup with the daemon worker lifecycle during Task 6.
4. Return packed vectors and passage metadata to the local index. Keep full row text out of resident search arrays.

Verification:

- Run: `go test ./internal/conversation/localsearch/... -count=1`. Run the production encoder with network access disabled on the Task 4 query sample.
- Expect: Offline startup succeeds. The asset digest and output dimension match the manifest. Passages cover a long row's first, middle, and last tokens. Tool and thinking passages retain the original message index.

### 5. Persist and refresh the local index (CLYDE-754)

Depends on: Tasks 2 and 4 for storage. Daemon integration depends on Task 1.

Files:

- Create: `internal/conversation/localsearch/store.go`, `internal/conversation/localsearch/refresh.go`, `internal/conversation/localsearch/search_index.go`.
- Modify: `internal/config/paths.go`, `internal/conversation/index.go`, `internal/daemon/conversation_semantic_sync.go`, `internal/daemon/initial_index.go`, `internal/daemon/hard_reset.go`, `internal/daemon/initial_index_test.go`.

Behavior:

- Persist one versioned SQLite record per conversation under `config.GlobalCacheDir()`. SQLite is already a Clyde dependency. Each record stores the source fingerprint, projection hash, and a packed payload of passage vectors and scalar metadata. A transaction replaces the record after encoding completes. A failed transaction preserves the prior record. Task 7 measures database overhead.
- Startup loads the last complete generation as packed vectors and compact metadata in RAM. Search scoring reads that generation without SQLite calls or full transcript text. Refresh publishes changed conversation blocks after their transactions commit. Search uses the prior generation until publication.
- The existing sync worker asks the local adapter for needed conversation IDs. A changed mtime with unchanged projection skips encoding. Only a confirmed removal from a completed raw refresh deletes local passages. Hidden, empty, deferred, and failed-load records are not removals. A missing, corrupt, or incompatible index rebuilds from the raw index. It never selects LMS. The current worker loads an oversized artifact alone; its 8 MiB batch target does not bound that raw load.

Steps:

1. Add a local cache path and schema metadata for index version, model digest, tokenizer digest, passage rules, vector encoding, and selected content policy. Open the database only in local mode.
2. Implement transactional conversation replacement and removal. Bound model input batches independently of raw artifact size. Measure peak memory for the largest raw conversation. Extend the existing index reader and provider parsers for incremental message loading if the whole-conversation load exceeds the 16 GB target; do not create parallel parsers. Store no transcript text or provider-owned paths in vector payloads.
3. Load vectors once at startup. Build provider, conversation, workspace, role, time, and archive candidate restrictions from typed metadata. Do not copy vectors into each filter structure. Extend `conversation.Index` with a completed-refresh membership signal before deleting local records; a filtered `ListWithStamps` result alone does not prove removal.
4. Change `RunInitialConversationIndex` to skip raw discovery when its cache exists and still build a missing local index from the loaded raw records. Dispatch the existing first-install `runInitialSemanticIndex` path by the configured backend after raw indexing. Local selection builds without dialing LMS, including when a raw cache already exists. Preserve selected LMS install behavior and progress reporting.
5. Include local cache files in the matching hard-reset scope. Detect corruption and schema mismatch before publishing a generation. Log rebuild progress and report pending freshness.

Verification:

- Run: `go test ./internal/conversation/localsearch/... ./internal/daemon/... -run 'Local|InitialConversationIndex|Semantic' -count=1`.
- Expect: Restart loads the committed index before refresh. One changed conversation replaces only its passages. Unchanged projected content skips encoding. An interrupted transaction leaves the prior index searchable. Confirmed raw removal stops returning the deleted conversation.

### 6. Connect local retrieval to CLI and MCP (CLYDE-755)

Depends on: Tasks 1, 3, and 5.

Files:

- Create: `internal/conversation/localsearch/search.go`, `test/live/harness_local_search_test.go`.
- Modify: `internal/daemon/conversation_search_source.go`, `internal/daemon/run.go`, `internal/daemon/control_server.go`, `internal/daemon/runtime_status.go`, `internal/daemon/client_status.go`, `test/live/harness_semantic_config_test.go`.
- Modify: `internal/conversation/search_result.go`, `api/clyde/v1/daemon/service.proto`, `internal/daemon/client.go`, and the CLI/MCP source rendering tests for the selected backend's public source and freshness.
- Modify: `docs/conversations.md` for backend selection and install behavior.

Behavior:

- The local backend scores only allowed candidates, applies minimum score after scoring, and returns ranked passage hits. Clyde groups winning hits by conversation and reads selected source fields through the existing loader. It renders the matched passage span, then uses the original message index and load rules for the context window.
- Clyde excludes hidden, missing, and disallowed archived records before paging. It deduplicates local passages from one message before per-conversation limits, offset, and page limit. Preserve LMS ordering during the generic cutover. Equal scores use a stable secondary key. CLI and MCP return the existing matches, facets, freshness, filter accounting, and `has_more` fields.
- Add a `SEARCH_SOURCE_LOCAL` value outside the proto's reserved 2 and 3 slots. Map it through domain, daemon response, and client rendering; leave `SEARCH_SOURCE_SEMANTIC` for LMS. `controlServer.SearchConversations` currently overwrites source freshness with its LMS snapshot. Select the local worker's snapshot for local results. Add the selected backend to `SemanticStatus`; report local index readiness as its connection state, with zero LMS retry counters in local mode.
- Local mode makes no LMS dial. Selected LMS returns the typed unavailable error when down and never opens the local index. Ingestion-only mode updates without serving search. Search-only mode reads an existing index without updating it.

Steps:

1. Implement bounded candidate scoring and stable ranking. Return row kind, passage ordinal, and source span for accurate text, tool, or thinking snippets.
2. Register local ingestion and search adapters at daemon startup. Attach model sessions and background work to `livetrack`. Preserve zero-bind-gap reload on backend changes.
3. Add an isolated live test with real temporary Claude, Codex, and Cursor artifacts, the bundled model, temporary XDG directories, the daemon, the CLI command, and the MCP tool. Assert user-visible results through both public boundaries.
4. Cover long text near a passage end, tool-only content, selected thinking, empty content, combined filters, archive visibility, pagination, restart, and selected LMS outage. Compare CLI and MCP conversation IDs and message indexes for the same request.
5. Document `backend`, both independent switches, local index location, install behavior, and selected-LMS outage behavior.

Verification:

- Run: `go test -tags live -count=1 ./test/live/ -run 'LocalSearch|SemanticConfig'`, then `make check`.
- Expect: CLI and MCP return the same public result shape and indexes. Local mode searches with LMS absent. Selected LMS outage returns an error. Raw listing, reading, context, and export still work.

### 7. Measure the complete corpus (CLYDE-756)

Depends on: Task 6.

Files:

- Create: `test/bench/localsearch_bench_test.go`, `docs/conversations/local-search-measurements.md`.
- Modify: `internal/conversation/localsearch/model.go`, `internal/conversation/localsearch/search.go`, or `internal/conversation/localsearch/store.go` only for a measured resource failure.

Behavior:

- Measure the complete Codex, Cursor, and Claude corpus with the final model and index format. Report conversation, row, and passage counts; model and index bytes; initial and incremental build times; startup time; peak build and steady search RSS; and p50 and p95 query latency. Test 16 GB and 24 GB machines or state the exact constrained representative environments.

Steps:

1. Record hardware, OS, model digest, index version, selected content policy, corpus counts, and measurement commands. Keep transcript text out of committed benchmark output.
2. Build a clean index, restart and load it, update one changed conversation, and run unfiltered and restrictive cross-provider queries. Measure each phase separately.
3. Compare actual size and memory with the earlier bare-vector estimate. Measure quality on Task 4's fixed query sample. Report low-recall cases.
4. Adjust batch size, model concurrency, metadata residency, or vector encoding only for a measured 16 GB or 24 GB failure. Repeat the same measurement and record both results.

Verification:

- Run: `go test ./test/bench -run '^$' -bench LocalSearch -benchmem` against the configured full corpus. Run `make check` after code adjustments.
- Expect: The report separates estimates from measured disk and RAM use. It includes the largest single-conversation load, repeatable startup and query numbers, and a result for both target memory configurations. Indexing and search complete without memory exhaustion on each configuration.
