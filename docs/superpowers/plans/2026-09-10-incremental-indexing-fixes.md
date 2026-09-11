# Incremental Indexing Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop unnecessary conversation and semantic work, preserve useful Cursor progress across restarts, keep request resolution and export usable without the daemon, and provide a real daemon uninstall command.

**Architecture:** Keep raw conversation discovery, semantic ingestion, semantic search, and daemon lifecycle as separate gates. Reuse the existing cache and refresh owner; add only source stamps, offsets, and signatures needed to skip unchanged work. Keep local export on the existing conversation readers and keep daemon uninstall on the existing native deployment path.

**Tech Stack:** Go, Cobra, SQLite read-only snapshots, TOML configuration, existing daemon lifecycle and native service installers, existing semantic client.

**Spec:** `docs/superpowers/specs/2026-09-08-incremental-daemon-work-design.md`

## Global Constraints

- `ingestion_enabled` and `search_enabled` are independent opt-ins and both default to `false`.
- The removed `conversation.semantic.enabled` key is a hard configuration error; do not add an alias or migration.
- Ingestion disabled must not start raw scanning, semantic runtime creation, socket checks, registration attempts, or retries.
- Search enabled with ingestion disabled may query already indexed semantic data.
- Preserve existing cache formats and ordinary writes; do not add durability, migration, journal, recovery, watcher, queue, or connection-pool work.
- Provider-owned files remain read-only.
- Do not touch LMS, other repositories, hooks, MCP configuration, credentials, provider databases, or installed binaries during daemon uninstall.
- Keep the change to the eight reported bugs and the requested foreground install index.
- Use one pull request with four focused commits. Each commit must pass its focused tests before the next slice starts.
- Verified ticket tags are `CLYDE-708` for configuration, `CLYDE-717` for performance validation, and GitHub issue `#315` for the remaining regression slices. Do not invent additional ticket identifiers.

---

### Task 1: [CLYDE-708] Gate work and show initial install indexing

**Files:**
- Modify: `internal/config/conversation_config.go`, `internal/config/load_removed_conversation.go`, and their existing tests only if the current hard-cut behavior is incomplete.
- Modify: `internal/daemon/run.go`
- Create: `internal/daemon/initial_index.go`
- Create: `internal/daemon/initial_index_test.go`
- Modify: `internal/clispec/daemon_ops.go`
- Modify: `internal/daemon/hard_reset_test.go` fixtures so enabled behavior is explicit.

**Interfaces:**
- Consumes: `config.Conversation.Semantic.IngestionEnabled`, `SearchEnabled`, the existing conversation index, and the existing semantic client.
- Produces: `RunInitialConversationIndex(ctx context.Context, output io.Writer, progress func(completed, total int)) error` and the native install call that invokes it before daemon registration.

- [ ] **Step 1: Write the failing gating tests.**

  Add end-to-end daemon startup assertions for these exact combinations:

  ```go
  {ingestion: false, search: false, wantRawScan: false, wantSemanticProbe: false}
  {ingestion: false, search: true, wantRawScan: false, wantSemanticProbe: true}
  {ingestion: true, search: false, wantRawScan: true, wantSemanticProbe: true}
  {ingestion: true, search: true, wantRawScan: true, wantSemanticProbe: true}
  ```

  Assert that a search-only request can use an existing semantic client without creating a raw refresh worker.

- [ ] **Step 2: Run the focused tests and verify the old behavior fails.**

  Run:

  ```bash
  go test ./internal/config ./internal/daemon -run 'Test.*Semantic|Test.*Index'
  ```

  Expected failure: the ingestion-disabled path starts raw discovery or performs a semantic availability attempt.

- [ ] **Step 3: Implement the smallest gate.**

  Start the raw index worker only when `IngestionEnabled` is true. Keep semantic search construction independent. When both flags are false, construct neither the semantic runtime nor its retry worker. Preserve the current in-memory retry worker for an explicitly enabled runtime; the foreground install path must not create a second retry loop.

- [ ] **Step 4: Add foreground install indexing.**

  On a fresh native install with ingestion enabled:

  1. Print `Initial indexing: starting`.
  2. Run one raw refresh in the foreground.
  3. Print `Initial indexing: X/Y conversations` for the initial count and each completed conversation.
  4. Make one semantic client attempt.
  5. If the engine is unavailable, print one `Initial indexing: semantic unavailable: ...` line and continue.
  6. If semantic indexing succeeds, submit the existing bounded sync pass.
  7. Print `Initial indexing: complete` only after the foreground work finishes.

  When ingestion is disabled, print the exact skip reason and do not open provider stores or probe the semantic socket. If the initial index is already complete, retain the existing fast path and do not repeat the install work.

- [ ] **Step 5: Test the installer output and single semantic attempt.**

  Add these public-behavior tests:

  ```go
  TestRunInitialConversationIndexReportsProgress
  TestRunInitialConversationIndexSkipsWhenIngestionDisabled
  TestRunInitialConversationIndexTriesSemanticOnce
  TestNativeInstallRunsInitialIndexBeforeRegistration
  ```

  Assert the exact start, progress, complete, skip, and unavailable strings. Replace the semantic dial function in the test with a counting function and assert one call.

- [ ] **Step 6: Run the slice checks.**

  ```bash
  go test ./internal/config ./internal/daemon ./internal/clispec
  go test ./... -run 'Test.*Semantic|Test.*Initial|Test.*HardReset'
  ```

- [ ] **Step 7: Commit the first slice.**

  ```bash
  git add internal/config internal/daemon internal/clispec
  git commit -S -m "Gate conversation indexing and show install progress"
  ```

### Task 2: [CLYDE-717] Reuse unchanged Cursor discovery and transcript state

**Files:**
- Modify: `internal/conversation/parser.go`
- Modify: `internal/conversation/scan.go`
- Modify: `internal/providers/cursor/parser/parser.go`
- Create: `internal/providers/cursor/parser/cached_discovery.go`
- Modify: `internal/providers/cursor/parser/resume_links.go`
- Modify: `internal/providers/cursor/store/discovery.go`
- Test: existing Cursor parser, discovery, append, and integration test files.

**Interfaces:**
- Consumes: the existing conversation cache, `FileStamp`, SQLite read-only discovery, and current parser readers.
- Produces: cached discovery through the internal `CachedDiscoveryParser` interface, persisted transcript and store stamps, bounded metadata signatures, and unchanged-source skips.

- [ ] **Step 1: Add failing restart and unchanged-source tests.**

  Add tests that run discovery twice with the same cache and assert:

  - unchanged JSONL transcripts decode zero records on the second run;
  - unchanged Cursor databases, WAL files, workspace descriptors, and metadata stores open zero additional readers on the second run;
  - a changed transcript decodes only appended complete lines;
  - a partial final line remains for the next pass;
  - a changed WAL or descriptor causes one normal refresh;
  - a remote `vscode-remote://` workspace remains readable;
  - an unchanged invalid descriptor reports once and is not reparsed on every pass.

- [ ] **Step 2: Run the focused tests to capture the current failure.**

  ```bash
  go test ./internal/conversation ./internal/providers/cursor/parser ./internal/providers/cursor/store -run 'Test.*(Cache|Restart|Append|Descriptor|Discovery|Workspace)'
  ```

- [ ] **Step 3: Cache source discovery before content reads.**

  Dispatch `CachedDiscoveryParser.DiscoverCached` from the existing scan owner. For each source, compare the existing path, size, modification time, WAL stamp, descriptor stamp, and workspace identity before opening content readers. Reuse the cached record and stamp when unchanged. Keep failed or unreadable sources eligible for a later ordinary pass.

- [ ] **Step 4: Resume transcript reads from saved offsets.**

  Reuse existing append offsets and saved lineage, headers, and links. Decode complete new records only. Restart from the beginning on truncation or replacement. Do not add prefix hashes, parser journals, or durability synchronization.

- [ ] **Step 5: Replace full `composerData` hashing.**

  Compute the existing SQLite signature from row count, last row position, timestamps, flags, byte lengths, and the selected JSON fields using SQL aggregates. Do not read or hash the full `composerData` payload. Keep the existing read-only connection boundary.

- [ ] **Step 6: Run Cursor tests and verify counters.**

  ```bash
  go test ./internal/conversation ./internal/providers/cursor/parser ./internal/providers/cursor/store
  ```

  Assert `ObserveDiscoveryReads` and parser decode counters for unchanged, appended, WAL-changed, descriptor-changed, and replaced sources.

- [ ] **Step 7: Commit the second slice.**

  ```bash
  git add internal/conversation internal/providers/cursor/parser internal/providers/cursor/store
  git commit -S -m "Reuse unchanged Cursor discovery state"
  ```

### Task 3: [Issue #315, request and export slice] Resolve requests before refresh and export locally

**Files:**
- Modify: `internal/conversation/index.go`
- Modify: `internal/daemon/conversation_index.go`
- Modify: `internal/clispec/conversation_export.go`
- Test: `internal/conversation/request_index_test.go`, `internal/daemon/conversation_index_test.go`, and the existing export tests.

**Interfaces:**
- Consumes: cached conversation records, the bounded provider request lookup, and the existing local transcript exporter.
- Produces: request resolution that refreshes only when cached data cannot map the selector, and CLI export that does not require a daemon RPC.

- [ ] **Step 1: Add failing request-resolution tests.**

  Assert that a request selector maps to the cached conversation before any refresh, and that a refresh occurs only when the bounded lookup cannot map it. Assert that duplicate ambiguity keeps the existing refresh behavior.

- [ ] **Step 2: Add the stopped-daemon export test.**

  Run the public conversation export command against a temporary provider fixture while no daemon socket exists. Assert the command writes the expected transcript and does not attempt a daemon connection.

- [ ] **Step 3: Implement the bounded lookup order.**

  Query the provider's bounded request index first when the cache has no carrier. Return immediately when the result maps to a cached native conversation. Call the existing refresh method only when the mapping is absent or ambiguous. Do not turn request lookup into a full provider scan.

- [ ] **Step 4: Route CLI export to the local reader.**

  Keep MCP export on its daemon RPC. Route only the local CLI command to `ExportTranscriptLocal`, using the same cache and provider readers already used by conversation operations.

- [ ] **Step 5: Run the slice checks.**

  ```bash
  go test ./internal/conversation ./internal/daemon ./internal/clispec -run 'Test.*(Request|Resolve|Export|Transcript)'
  ```

- [ ] **Step 6: Commit the third slice.**

  ```bash
  git add internal/conversation/index.go internal/daemon/conversation_index.go internal/clispec/conversation_export.go internal/conversation/request_index_test.go internal/daemon internal/clispec
  git commit -S -m "Resolve cached requests and export without daemon"
  ```

### Task 4: [Issue #315, daemon lifecycle slice] Add explicit daemon uninstall

**Files:**
- Create: `internal/clispec/daemon_uninstall.go`
- Create: `internal/clispec/daemon_uninstall_test.go`
- Modify: `internal/clispec/registry.go`
- Modify: `internal/clispec/alignment_test.go`
- Create: `internal/daemon/uninstall.go`
- Modify: `internal/deploy/remove.go`
- Modify: `internal/deploy/remove_test.go`

**Interfaces:**
- Consumes: Cobra's existing daemon command group and the native deploy removal helpers.
- Produces: `clyde daemon uninstall` help by default and `clyde daemon uninstall --apply` that stops and unregisters only Clyde's daemon.

- [ ] **Step 1: Add the no-op CLI test.**

  Execute `clyde daemon uninstall` without `--apply`. Assert that it prints the command help, returns the explicit apply-required error, and leaves service registration and files unchanged.

- [ ] **Step 2: Add the apply lifecycle test.**

  Use the existing native deployment fixture. Assert that apply stops the daemon, removes only Clyde's registration, and leaves config, data, logs, hooks, MCP configuration, provider stores, repositories, and binaries untouched.

- [ ] **Step 3: Implement the command.**

  Register the command in the shared CLI specification. Require `--apply` before mutation. Reuse the Go deploy removal path for macOS and Linux. Do not use broad process-name matching. Keep the command separate from hard reset, which owns database deletion and reinstall.

- [ ] **Step 4: Run lifecycle and alignment checks.**

  ```bash
  go test ./internal/clispec ./internal/daemon ./internal/deploy
  go run ./cmd/clyde daemon uninstall --help
  go run ./cmd/clyde daemon uninstall
  ```

  The second command must print help and make no mutation.

- [ ] **Step 5: Commit the fourth slice.**

  ```bash
  git add internal/clispec internal/daemon/uninstall.go internal/deploy
  git commit -S -m "Add explicit daemon uninstall command"
  ```

### Task 5: [CLYDE-717] Measure the change on the same live corpus

**Files:**
- Create outside the repository: a temporary measurement harness under `/tmp/clyde-index-measurement`.
- Record in the pull request description: the resulting metrics table and raw command logs.

**Interfaces:**
- Consumes: the current live provider corpus, the pre-change `origin/main` binary, the candidate binary, existing discovery counters, process resident memory, and Go allocation profiles.
- Produces: matched three-minute samples for disabled, search-only, and ingestion-enabled modes before and after the change.

- [ ] **Step 1: Freeze the measurement contract.**

  Use the same corpus, config roots, request mix, refresh cadence, and machine for both binaries. Do not write provider files. Run each mode for 180 seconds after a two-minute warmup.

- [ ] **Step 2: Measure the pre-change binary.**

  Build `origin/main`, run the existing daemon sandbox with each configuration, and record refresh count, raw scan count, Cursor database opens, transcript decodes, resident memory, CPU, and allocation profile.

- [ ] **Step 3: Measure the candidate binary.**

  Run the identical three samples against the same corpus and capture the same counters. Keep each mode's logs separate.

- [ ] **Step 4: Apply the acceptance gate.**

  Accept only when disabled and search-only modes show zero raw scans, unchanged restart shows zero Cursor decoding, and the `composerData` signature allocation hotspot is absent. Report any failed gate with the exact counter and mode.

### Task 6: Final verification and one pull request

- [ ] **Step 1: Run all tests.**

  ```bash
  go test ./...
  make build
  make check
  git diff --check
  ```

- [ ] **Step 2: Inspect the four commits.**

  Confirm each commit contains only its tagged slice, has a signed commit object, and leaves the pre-existing untracked `install` file untouched.

- [ ] **Step 3: Write the pull request description.**

  Include the breaking configuration notice, the four slice tags, the eight bug fixes, the exact install progress output, the uninstall safety boundary, and the before/after measurement table. State that the required operator migration is the approved hard reset and that LMS is untouched.

- [ ] **Step 4: Submit one pull request and verify it.**

  Use the Graphite stack workflow for the four commits. Recheck the diff, required checks, review comments, installed artifact, and live behavior before claiming completion. Mark the Tack epic and its started issues in progress, then mark them done only after the live proof passes.

## Self-review coverage

- Raw scans are gated by ingestion: Task 1.
- Search-only operation remains independent: Task 1.
- Transcript stamps, offsets, lineage, headers, and restart reuse: Task 2.
- Cursor database, WAL, workspace, descriptor, and metadata reuse: Task 2.
- Bounded SQLite signature instead of full `composerData` hashing: Task 2.
- Bounded request lookup before refresh: Task 3.
- Local export with the daemon stopped: Task 3.
- Explicit daemon stop and unregister command: Task 4.
- Foreground install indexing with progress and one unavailable-engine attempt: Task 1.
- Three-minute before and after measurement: Task 5.
- No durability or unrelated defense-in-depth work: Global Constraints and every task boundary.
