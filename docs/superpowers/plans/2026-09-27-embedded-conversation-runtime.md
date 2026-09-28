# Clyde runtime and acceptance plan

The [shared coordination plan](2026-09-27-shared-search-coordination.md) is the sole execution order and acceptance ledger. The [Clyde design](../specs/2026-09-27-embedded-search-design.md) defines the product and library contract. This lane owns only the files and tests below.

## C4.1. Bootstrap the native dependency workspace

Depends on: LMS L0 native packaging gate. This is a separate commit and PR before C1 compilation. C2 and C3 do not depend on the later runtime integration.

Files:

- Modify: [go.mod](../../../go.mod), [go.sum](../../../go.sum), and [Makefile](../../../Makefile).
- Create: `third_party/gksyntax` as a pinned recursive submodule when the public library import requires the shell grammar package.

Steps:

1. Pin the exact LMS library commit and `gksyntax` source revision. Import only the public packages that Clyde uses. Verify which native libraries those imports require before adding CGO dependencies.
2. Add the pinned `third_party/gksyntax` recursive submodule and set `GO_MK_GENERATE := gksyntax-grammars`, `GO_MK_GENERATE_INPUTS := third_party/gksyntax`, and `GO_MK_WORKSPACE_USE := . third_party/gksyntax` in the Clyde Makefile. The grammar target initializes the recursive submodule, runs `third_party/gksyntax/scripts/install-tree-sitter.sh .bin`, and generates the Swift parser, matching the committed LMS build. Add only required values to `GO_MK_CGO_DEPS` and `GO_MK_CGO_CACHE_VERSIONS`. Bind `make test`, `make check`, and `make embedded-search-live` to `$(GO_MK_PREREQS)`. Fail when a pinned source, compiler, or required native library is unavailable. Use the configured Go workspace for builds. Run `GOWORK=off` only as an optional packaging check after the module archive becomes self-contained.
3. Define `embedded-search-bootstrap` with `$(GO_MK_PREREQS)` as prerequisites. Compile the package imports used by C1 and the library facade on macOS and Linux. Record the pinned revisions and build commands in the PR.

Verification:

- Run: `make embedded-search-bootstrap` and `go test ./internal/daemon -run '^$'` in the configured pinned workspace.
- Expect: The native parser and library imports compile on supported macOS and Linux hosts without an LMS daemon.

## C4.2. Connect configuration and daemon lifecycle

Depends on: C4.1, C2, C3, LMS L1, and LMS L3.

Files:

- Modify: [internal/config/conversation_config.go](../../../internal/config/conversation_config.go).
- Modify: [internal/config/load.go](../../../internal/config/load.go) for typed validation and [internal/config/change_class.go](../../../internal/config/change_class.go) for `RouteReload` classification.
- Modify: [internal/daemon/conversation_semantic_runtime.go](../../../internal/daemon/conversation_semantic_runtime.go).
- Modify: [internal/daemon/run.go](../../../internal/daemon/run.go).
- Modify: [internal/daemon/runtime.go](../../../internal/daemon/runtime.go) and [internal/daemon/lifecycle_group.go](../../../internal/daemon/lifecycle_group.go) for library resource registration and shutdown.
- Modify: [internal/daemon/control_server.go](../../../internal/daemon/control_server.go).
- Modify: [internal/daemon/runtime_status.go](../../../internal/daemon/runtime_status.go) and [internal/daemon/client_status.go](../../../internal/daemon/client_status.go).
- Modify: [internal/conversation/semsearch/client.go](../../../internal/conversation/semsearch/client.go) for complete removal after callers migrate.
- Create: `test/live/embedded_conversation_search_test.go` for end-to-end CLI and MCP behavior.
- Create: `test/live/embedded_search_harness.go` for isolated Milvus and SQLite setup using the existing live harness.
- Create: `test/live/embedded_search_acceptance_test.go` for full-corpus measurements and deterministic query IDs.
- Create: `cmd/embedded-search-acceptance/main.go` for the report comparison command.
- Modify: [test/live/harness.go](../../../test/live/harness.go) and [test/live/conversation_config.toml.tmpl](../../../test/live/conversation_config.toml.tmpl) to enable semantic read and write only for the isolated embedded search cases and configure the library catalog, Milvus adapter, and embedder.
- Modify: [Makefile](../../../Makefile) for strict live and acceptance targets.
- Modify: [docs/conversations.md](../../../docs/conversations.md) after the runtime behavior passes tests.

Behavior:

- Implement the keys, types, and defaults in the proposed configuration table. Preserve independent ingestion and search switches. Existing default content remains chat plus tool calls; archived, subagent, tool output, and thinking content remain excluded. Resolve the global `conversation.include_subagent_conversations` raw index setting with semantic selection so an enabled semantic setting can select subagent sources without making them visible in ordinary list results.
- Create and own the Milvus SDK client in Clyde. Inject `library/milvus` as the vector adapter and open the library once per daemon generation. The library also retains an offline `library/embedded` vector adapter for code search; Clyde's Milvus choice does not remove that profile. Stop ingestion before closing the library and Clyde's Milvus client during reload and shutdown. Recover after a transient unavailable store without selecting a different backend. Report connection and indexing status without an LMS socket or daemon process.
- Bind each catalog and vector pool generation to one resolved model, revision, dimension, and normalization descriptor. Treat `ErrStoreMismatch` as a configuration failure. A model setting change creates a new configured generation, builds it without modifying the prior one, and publishes it only after searchable completion. Preserve the prior generation for reads and recovery. Do not delete either generation automatically.
- Remove all conversation search and ingest RPC calls from Clyde after C2 and C3 pass. Keep CLI and MCP entry points. Execute LMS L5 after the integrated Clyde and codebase candidates pass joint acceptance.

Steps:

1. Add typed configuration and validation. Reject unknown selectors and impossible dimensions before daemon startup. Keep the current default policy and independent read and write switches.
2. Register library workers with `livetrack.Attach`. Extend `installConversationSemanticSyncStop` and `installRetryStop` to cancel and join ingestion before `PhaseWorkers` drains. Register library and client closure at `PhaseStorage`, after workers. Use `Group.Quiesce` through `runtimeServices.shutdown`. Reload starts the replacement before draining the old process; acquire the shared writer lock per publication or recovery operation, never for the runtime lifetime. Verify worker exit and lock release after reload, including a timed-out stop hook. Update status and error mapping, then remove unused gRPC client code.
3. Add `make embedded-search-live`. It runs the C1, C2, C3, and C4 tests with native prerequisites and real Milvus. The target fails when a dependency is absent, a named test matches zero cases, or a case skips. Use temporary stores and keep the LMS daemon absent. Verify the same ordered results through CLI and MCP, then restart the daemon and repeat the query. Test ingestion-only and search-only switches separately.
4. Add `make shared-search-acceptance` with required `CORPUS_SNAPSHOT`, `QUERY_BATTERY`, `BASELINE_REPORT`, and `REPORT_PATH` absolute paths. The isolated harness restores an immutable corpus snapshot. A query battery JSON entry contains `id`, `query`, typed `filter`, `page_size`, `expected_occurrence_ids`, and `expected_total`; the battery also declares concurrency and timeout. A report contains schema version, build revision, corpus digest, hardware identity, model descriptor, battery digest, concurrency, per-query ordered IDs and page counts, selected and excluded counts by provider and content kind, distinct embeddings, vector writes and reuse, ingestion stage durations, model startup and throughput, latency distribution, peak RSS, and disk bytes. A timeout, short page, missing expected ID, or partial result fails the run.
5. Capture a baseline with `make shared-search-baseline CORPUS_SNAPSHOT=/absolute/path/corpus QUERY_BATTERY=/absolute/path/queries.json BASELINE_BINARY=/absolute/path/existing-clyde REPORT_PATH=/absolute/path/baseline.json` against isolated stores. The harness invokes the existing binary's public CLI and writes the same report schema. Compare the candidate with `make shared-search-acceptance`; reject a missing baseline or a corpus, hardware, model, battery, or concurrency compatibility hash mismatch. Report measured changes separately from the accepted verdict. Fail for latency or RSS regression under the sole coordinator's thresholds. Never run either target on production stores.
6. Normalize baseline and candidate hit identities to provider conversation ID, original message index, content kind, tool index, and part span from the frozen source manifest. Do not compare incompatible legacy and library storage keys as if they identify different source content. Test first ingestion from an existing raw cache, source truncation, missing artifacts, semantic-off mode, and routine hard reset without deleting committed occurrences.

Verification:

- Run: `make test`, `make check`, and `make embedded-search-live` in the configured pinned workspace.
- Run: `make shared-search-acceptance CORPUS_SNAPSHOT=/absolute/path/corpus QUERY_BATTERY=/absolute/path/queries.json BASELINE_REPORT=/absolute/path/baseline.json REPORT_PATH=/absolute/path/candidate.json` in the isolated full-corpus workspace.
- Expect: CLI and MCP return the same complete ordered hits. Search-only and ingestion-only modes work independently. Restart preserves committed search. A failed Milvus or store operation returns a typed error. Native builds pass on supported platforms.
