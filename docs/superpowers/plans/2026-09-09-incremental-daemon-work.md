# Daemon Performance and Bug Fixes Implementation Plan

> **For agentic workers:** Use superpowers:subagent-driven-development or superpowers:executing-plans task by task. Checkboxes record execution; none are completed by writing this plan.

**Goal:** Reduce demonstrated background CPU and disk work, fix semantic opt-in
and Cursor URI bugs, and expose the status requested in issue 315.

**Architecture:** Extend the existing refresh worker, provider readers, semantic
retry loop, and metrics worker. Cache unchanged inputs and batch ordinary writes.
Do not add new scheduling, durability, or recovery frameworks.

**Tech stack:** Go 1.26.5, existing SQLite readers, gRPC, and TOML configuration.

**Spec:** [Performance and bug-fix scope](../specs/2026-09-08-incremental-daemon-work-design.md).
The user narrowed the prior design on September 9, 2026. This plan supersedes the
previous durability and crash-recovery tasks.

## Scope rules

- Implement observed bugs and likely ordinary user behavior only. The spec's
  evidence table determines inclusion.
- Performance takes priority over durable Clyde-owned caches and summaries.
  Recent derived state may be lost or rebuilt after termination.
- Do not add file/directory sync, journals, backup generations, transactional
  checkpoint readers, delivery IDs, collection epochs, or crash tests.
- Keep provider-owned artifacts read-only. Preserve normal conversation content,
  identifiers, ordering, archive changes, and metadata.
- Use the current isolated checkout and leave the untracked `install` artifact
  alone. Do not install, deploy, merge, or change the production daemon.
- Keep existing lifecycle ownership for normal shutdown and reload. Do not build
  new handoff protocols or generalized outage handling.
- Both `ingestion_enabled` and `search_enabled` default independently to false.
  Ingestion off must not prevent search over stored conversations.
- Run targeted tests and `make check` before implementation commits. Existing
  formatter failures are ordinary check prerequisites, not a separate feature.
  Do not weaken checks or include unrelated refactors.
- Commit logical changes with `git commit -S` and the Codex co-author trailer.
  No implementation task is completed merely by updating this document.

## Task dependencies

| Task | Deliverable | Depends on |
| --- | --- | --- |
| 1 | Independent semantic opt-ins and configuration hard cut | None |
| 2 | Less frequent missing-engine retries and no repeated warning flood | 1 |
| 3 | Correct remote workspace parsing | None |
| 4 | Shared cached Cursor discovery | 3 |
| 5 | Fewer refreshes, unchanged-cache writes, and transcript prefix rereads | 4 |
| 6 | Incremental metrics reads and ordinary checkpoint writes | None |
| 7 | Bounded semantic preparation and evidence for repeated work | 1, 2, 5 |
| 8 | Passive semantic and listener status | 1, 2 |
| 9 | Matched measurements, regression checks, and breaking-change PR | 1-8 |

Tasks 3-6 can proceed independently of semantic-engine installation.
No receiver protocol project is a prerequisite for this change.

### Task 1: Make ingestion and search independent

**Modify:** [Semantic configuration](../../../internal/config/conversation_config.go),
[loader](../../../internal/config/load.go),
[direction tests](../../../internal/config/conversation_semantic_directions_test.go),
[removed-key tests](../../../internal/config/load_removed_keys_test.go),
[example](../../../clyde.example.toml),
[daemon wiring](../../../internal/daemon/run.go),
[sandbox](../../../internal/cli/daemon/sandbox.go),
[live harness](../../../test/live/harness.go),
[live template](../../../test/live/conversation_config.toml.tmpl),
[harness tests](../../../test/live/harness_semantic_config_test.go), and
[update probe configuration](../../../cmd/ci-auto-update/main.go).
Update typed references to the old semantic field without renaming unrelated
`Enabled` fields.

**Contract:** `FeedsEngine()` returns ingestion intent. `AnswersSearch()` returns
search intent. `UsesEngine()` remains their logical OR. Both zero values are off.

- [ ] Add real-loader cases for omitted section, explicit false, ingestion only,
  search only, and both true. Search-only must leave ingestion off. Preserve the
  existing test's stored-search behavior by making its opt-in explicit.
- [ ] Run the cases before changing code; the omitted-search case currently
  resolves to true and must fail the new expectation.
- [ ] Replace the semantic fields and their accessors with independent booleans:

  ```go
  // Fields on ConversationSemanticConfig:
  IngestionEnabled bool `json:"ingestionEnabled,omitempty" toml:"ingestion_enabled,omitempty"`
  SearchEnabled bool `json:"searchEnabled,omitempty" toml:"search_enabled,omitempty"`

  func (semantic ConversationSemanticConfig) FeedsEngine() bool {
      return semantic.IngestionEnabled
  }

  func (semantic ConversationSemanticConfig) AnswersSearch() bool {
      return semantic.SearchEnabled
  }

  func (semantic ConversationSemanticConfig) UsesEngine() bool {
      return semantic.FeedsEngine() || semantic.AnswersSearch()
  }
  ```

- [ ] Reject scoped `enabled` before normal TOML decoding can ignore it, including
  when false or mixed with new keys. Name `ingestion_enabled` in the error.
  Keep `search_enabled` valid. Do not create aliases or an automatic converter.
  Apply the same rule to JSON only where configuration JSON is already accepted.
- [ ] Update producers, examples, and initializers. Default sandbox operation uses
  neither engine feature; explicit sandbox flags opt into ingestion and search.
  Tests for missing settings must omit them, not write both false.
- [ ] Use an isolated daemon with a counting Unix-socket fixture. With both off,
  run raw listing, reading, context, export, status, and disabled search requests.
  Assert zero engine calls. With ingestion off/search on, return one stored result
  and assert zero manifest and upsert calls.
- [ ] Test ordinary config reload and rejection of removed keys. Retain existing
  behavior that rejected configuration does not replace the running configuration.
- [ ] Run `go test ./internal/config ./internal/cli/daemon ./internal/daemon`,
  targeted live configuration tests, and `make check`. Commit as
  `Make semantic ingestion and search independent opt-ins`.

### Task 2: Reduce the reported missing-engine retry loop

**Modify:** [Semantic runtime](../../../internal/daemon/conversation_semantic_runtime.go),
[engine client](../../../internal/conversation/semsearch/client.go),
[search source](../../../internal/daemon/conversation_search_source.go),
[search errors](../../../internal/daemon/conversation_search_errors.go),
[sync worker](../../../internal/daemon/conversation_semantic_sync.go), and
[existing recovery tests](../../../internal/daemon/conversation_semantic_recovery_test.go).

**Contract:** Reuse the existing shared connector, single-flight lock, and retry
worker. No new persistent availability state or generalized reconnect service.

- [ ] Preserve the construction guard from Task 1: both false means no runtime.
  Disabled queries return disabled; opted-in queries without a client return
  unavailable promptly. Neither query path initiates a retry.
- [ ] Change the existing retry progression to the approved delays:

  ```go
  func semanticRetryDelay(failures uint32, jitter time.Duration) time.Duration {
      delay := 30 * time.Second
      for remaining := failures; remaining > 1 && delay < 5*time.Minute; remaining-- {
          delay = min(2*delay, 5*time.Minute)
      }
      return min(delay+max(jitter, 0), 5*time.Minute)
  }
  ```

  Invoke after at least one failure. Generate jitter between zero and one fifth
  of the base delay, capped by the function. Keep the existing 10-second attempt
  deadline. State is in memory and may reset when the process restarts.

- [ ] Keep the existing startup, connector, and lifecycle paths. Limit changes
  to opt-in, retry scheduling, and warning ownership. Do not introduce a broader
  connection-health or asynchronous-startup redesign without a reproduced bug.
- [ ] Keep the existing nil-client guard before ingestion preparation. Remove
  duplicate warnings for the same registration error from lower helpers; the
  runtime reports entry into unavailable state and recovery once. Repeated
  identical failures increment counters without warning on every attempt.
- [ ] Extend existing retry tests to assert the base progression, one shared
  attempt, prompt query return, and cancellation when both flags become false.
  Reuse local test timing controls; do not add timer persistence or socket watchers.
- [ ] Observe an opted-in daemon with an absent fixture engine while making
  repeated queries and status calls. They must not increase attempt frequency.
  Keep the existing ordinary later-registration-success test.
- [ ] Run the targeted semantic/config tests and `make check`. Commit as
  `Back off missing semantic engine registration without repeated warnings`.

### Task 3: Fix remote Cursor workspace parsing

**Modify:** [Cursor paths](../../../internal/providers/cursor/store/paths.go),
[registry](../../../internal/providers/cursor/store/registry.go), and their
existing descriptor tests.

**Contract:** Recognize remote schemes before local file-URI parsing. Preserve
remote identity and existing local-path behavior.

- [ ] Add the failing public-boundary case:

  ```go
  func TestRemoteWorkspaceAuthoritySurvivesDescriptorRead(t *testing.T) {
      descriptor := filepath.Join(t.TempDir(), "workspace.json")
      body := []byte(`{"folder":"vscode-remote://ssh-remote%2Bexample/path"}`)
      if err := os.WriteFile(descriptor, body, 0o600); err != nil { t.Fatal(err) }
      got, err := ReadWorkspaceFolderPath(descriptor)
      if err != nil { t.Fatal(err) }
      if got != "vscode-remote://ssh-remote%2Bexample/path" {
          t.Fatalf("workspace identity = %q", got)
      }
  }
  ```

- [ ] Run it and verify the current invalid URL escape failure.
- [ ] Extend the existing parser rather than writing a second URI parser. Remote
  workspace identities must not become local filesystem paths. Preserve local
  escaped spaces and ordinary empty-window behavior.
- [ ] Keep readable conversations when a descriptor cannot be decoded. Emit the
  error at one owning boundary; Task 4 caches unchanged descriptor outcomes.
- [ ] Run the Cursor store/parser tests and `make check`. Commit as
  `Preserve encoded remote Cursor workspace identities`.

### Task 4: Reuse unchanged Cursor discovery

**Modify:** [Cursor parser](../../../internal/providers/cursor/parser/parser.go),
[composer scan](../../../internal/providers/cursor/parser/composer_scan.go),
[bubble stock](../../../internal/providers/cursor/store/stock.go),
[request lookup](../../../internal/providers/cursor/store/request.go), and
the store paths/registry from Task 3.

**Contract:** Share the current inventory and decoded results across consumers.
Keep the existing minute refresh; do not introduce a watcher framework, persistent
connection pool, row-change feed, or generic discovery protocol.

- [ ] Extend `TestParserDiscoversScansAndStreamsCursorSources` using existing
  temporary global/workspace database helpers. Measure content-store opens and
  queries when composer, legacy, and request lookup use the same inventory.
- [ ] Cache per-store results by ordinary database, WAL, and descriptor stamps.
  Use named private structs and the existing metadata helpers. A main-database
  stamp alone is insufficient for SQLite WAL writes.
- [ ] Reuse unchanged results before opening databases or projecting message
  content. During a pass, share one changed-store read among existing readers.
  Cache empty results and unchanged descriptor errors too.
- [ ] Test ordinary appends, archive/title changes, WAL-only commits, added and
  removed workspaces, and an unreadable descriptor. Preserve prior contributions
  on an ordinary failed read; a failed read does not prove deletion.
- [ ] For a changed global store, combine repeated per-composer work where the
  existing query plan permits. Preserve orphaned stored messages, ordering, and
  metadata merge rules. Keep a full pass for changed stores when necessary;
  do not invent a protocol or integrity layer to avoid it.
- [ ] Measure unchanged and active stores separately. Require zero unchanged
  content reads; record the remaining active global-store cost honestly.
- [ ] Run `go test -race ./internal/providers/cursor/store ./internal/providers/cursor/parser -count=1`
  and `make check`. Commit as `Cache shared Cursor discovery before content reads`.

### Task 5: Remove unnecessary refresh and cache work

**Modify:** [Index](../../../internal/conversation/index.go),
[scan](../../../internal/conversation/scan.go),
[parser contracts](../../../internal/conversation/parser.go),
[resume links](../../../internal/providers/cursor/parser/resume_links.go),
[Cursor transcript decoder](../../../internal/providers/cursor/jsonl/transcript.go),
and the current daemon startup wiring.

**Contract:** Extend the existing refresh owner and cache. No new generations,
epochs, scheduler, durability helper, or persistence format.

- [ ] Make cached listing/status reads passive. Keep explicit `Refresh` and the
  existing single-flight waiter behavior. Reuse request-resolution tests for
  fresh results, not-found results, and duplicate request ambiguity.
- [ ] Carry a changed-result flag from scanning so an unchanged pass skips cache
  encoding and writing. Account for records, metadata, removed sources, and
  saved append progress. Both synchronous and background paths use the same rule.
- [ ] Keep the existing cache format and ordinary writer. Do not add sync calls,
  backups, transaction selectors, or failure-injection hooks. Do not repeat a
  source scan merely to obtain stronger persistence guarantees.
- [ ] Reuse existing complete-record offsets for normal transcript appends.
  Keep the partial final record for the next pass. Feed resume-link extraction
  from already-decoded records rather than rereading an unchanged parent prefix.
  Reset the source on ordinary truncation or detected replacement.
- [ ] Do not add prefix integrity hashes or serialized continuation journals.
  Retain existing startup behavior when saved cache/progress cannot be used.
- [ ] Remove lifecycle cancellation suppression in the background refresh path.
  Register and join the existing worker through the existing daemon lifecycle.
  Change parser cancellation boundaries only where required to stop that work;
  do not introduce a parallel parser hierarchy.
- [ ] Test repeated unchanged refreshes, one append, partial final lines, metadata
  changes, passive list reads, and ordinary reload. Assert returned data and
  actual cache-write/content-read counts.
- [ ] Run the affected conversation/provider tests, targeted ordinary reload
  tests, and `make check`. Commit as `Skip unchanged refresh persistence and prefix rereads`.

### Task 6: Resume metrics from ordinary file positions

**Modify:** [Metrics history](../../../internal/daemon/metrics_history.go),
[rollup](../../../internal/daemon/metrics_rollup.go),
[distiller](../../../internal/daemon/metrics_rollup_distill.go),
[worker](../../../internal/daemon/metrics_rollup_worker.go),
[report reader](../../../internal/daemon/metrics_rollup_report.go), and their tests.

**Contract:** Read new bytes, retain unfinished aggregates in memory, and write
simple offsets without coordinating them transactionally with output.

- [ ] Reuse `twoRequestLog`, `writeMetricsHistoryRecords`, and direct report
  comparison tests. Demonstrate that a second unchanged pass rereads old bytes
  before the change and reads no content afterward.
- [ ] Extend the existing checkpoint with per-source path/position metadata.
  Keep source offsets and pending request aggregates in the existing worker.
  Reuse the existing event parser and request/execution identity rules.
- [ ] Seek to the saved complete-line offset and process bounded new records.
  Advance over complete non-metric lines. Leave an incomplete final line for
  the next pass. Follow routine log rotation, reading the remaining known tail
  before moving to the new active file.
- [ ] Append summary output and save offsets with ordinary batched writes.
  No fsync, cross-file transaction, committed-length reader, shared reader lock,
  output generation, or persisted pending-request state is required.
- [ ] On startup, use compatible saved offsets. If absent or incompatible, start
  at the active log end and show unavailable historical summary coverage.
  Do not replay retained logs solely to rebuild derived state. Keep existing
  completed summaries readable; unfinished aggregates may be lost on restart.
  Ordinary status reports missing coverage rather than triggering a history scan;
  explicit historical requests retain their current behavior.
- [ ] Remember the next retention expiry in ordinary worker state. When nothing
  can expire, skip pruning and its temporary rewrite. Keep the existing retention
  period and normal pruning behavior when work is due.
- [ ] Test a completed request across ordinary passes, routine rotation,
  incomplete final lines, unchanged input, and due versus not-due retention.
  Compare normal-run totals with direct replay. Do not add crash or interrupted
  commit tests.
- [ ] Run `go test ./internal/daemon -run 'Rollup|MetricsHistory|Distill' -count=1`
  and `make check`. Commit as `Tail metrics logs without repeated history replay`.

### Task 7: Bound normal semantic preparation

**Modify:** The semantic sync worker and engine client from Task 2,
[projection](../../../internal/daemon/conversation_semantic_documents.go),
and existing content-policy, memo, pinning, and generation tests.

**Contract:** Keep the existing engine protocol and in-memory job tracking.
Reduce preparation size without adding durable delivery or outage machinery.

- [ ] Record needed conversations, prepared documents/bytes, batch sizes, and
  ordinary job outcomes on a representative backlog. Use revision/job evidence
  to distinguish actual repeats from progress through different conversations.
  The earlier 954,175 submitted records alone do not prove duplicates.
- [ ] Prepare complete conversations into bounded batches instead of collecting
  the entire needed set. Submit a batch before preparing the next. Process a
  conversation above the usual batch target alone using the current streaming
  path. Preserve message indexes and selected content.
- [ ] Keep the current active-job guard and receiver needed-list authority.
  A restarted process may submit work again. Do not add delivery IDs, journals,
  collection epochs, lost-acknowledgement handling, or a receiver capability gate.
- [ ] Reuse the existing projection/content memo. Test ordinary content and
  metadata changes. Change memo/deduplication behavior only when the test or
  workload trace reproduces a skipped update or unnecessary repeat.
- [ ] Keep batch framing and replacement semantics compatible with the current
  receiver. This task does not create a new chunking protocol or make exactly-once
  claims.
- [ ] Compare peak memory and preparation latency on the same ordinary backlog,
  with returned/indexed content as the correctness control.
- [ ] Run relevant semantic client/worker tests and `make check`. Commit as
  `Bound semantic conversation preparation batches`.

### Task 8: Expose the status requested in the issue

**Modify:** [Daemon status](../../../internal/daemon/status.go),
[control server](../../../internal/daemon/control_server.go),
[operation registry](../../../internal/clispec/daemon_ops.go),
[CLI renderer](../../../internal/cli/daemon/daemon.go), and existing control
protocol/status tests as needed.

**Contract:** Show effective flags, current connection state, next retry, and
existing listener addresses. Read current daemon-owned state without probes.

- [ ] Add only the typed status fields needed for those values. Reuse the current
  listener handles and control-server pattern. If a new status RPC is needed,
  limit its payload to this view rather than introducing configuration provenance
  journals, candidate generations, or a general lifecycle service.
- [ ] Keep disabled distinct from unavailable. Do not calculate effective flags
  using the CLI process's environment. Preserve the separate historical
  `--since` output shape.
- [ ] Show profiling disabled when no profiling listener was started; otherwise
  show its actual bound address and port. Keep existing profiling opt-in behavior.
  Do not redesign profiling lifecycle, bind validation, or descriptor inheritance
  in this performance change.
- [ ] Run repeated status calls while both features are disabled and while the
  opted-in engine is absent. Assert stable attempt counts and no new discovery
  or projection work. Verify the CLI and any exposed MCP operation agree.
- [ ] Run targeted status/registry tests and `make check`. Commit as
  `Show passive semantic and listener status`.

### Task 9: Measure the result and describe the breaking change

**Modify:** Existing isolated live tests and the closest behavior documentation:
[conversations](../../conversations.md), [Cursor stores](../../cursor/stores.md),
[metrics](../../logging/metrics.md), and [testing](../../testing/overview.md),
only where implemented behavior changes.

**Contract:** Verify the scoped bug fixes and normal workload improvement.

- [ ] Repeat matched quiet and active samples of at least five minutes on the
  same corpus. Run engine-disabled and explicitly opted-in cases separately.
  Include a normal transcript append, a WAL update, a metadata change, routine
  metrics rotation, and repeated list/status calls.
- [ ] Measure CPU, resident memory, logical source bytes/rows, OS-accounted disk
  bytes, cache writes, retry frequency, and preparation batch sizes. Calibrate
  CPU counter units and keep sampler overhead visible in the evidence.
- [ ] Require unchanged-source/cache work to disappear, disabled semantic calls
  to remain zero, retry logging to stop flooding, and normal returned content to
  remain correct. Measure active global-store costs separately.
- [ ] Run `make check`, `make test`, focused race tests where shared state changed,
  and the scoped isolated live cases. Do not add power-loss, corruption,
  interrupted-write, or extended outage combinations.
- [ ] Update behavior documentation without copying dated measurements into it.
  Do not claim durable writes, guaranteed recovery, or exactly-once delivery.
- [ ] Prepare the implementation PR through the repository workflow. Verify all
  branch-local commit signatures before pushing and include this notice:

  > Breaking change: Under `[conversation.semantic]`, replace `enabled` with
  > `ingestion_enabled`. Both `ingestion_enabled` and `search_enabled` default
  > independently to `false`. Set `search_enabled = true` to search existing
  > indexed conversations without ingestion. The removed `enabled` key causes a
  > configuration error, even when false. Existing TOML is not migrated automatically.

  State measured changes and which ordinary cases passed. Installation and
  deployment require separate authorization.

## Removed from the previous plan

The durable-file package, file/directory sync, atomic generation protocol,
transactional metrics checkpoints, reader leases, historical state migration,
delivery journals, receiver epoch/idempotency extensions, serialized retry
cooldowns, prefix integrity verification, new scheduler/watchers, and profiling
hardening are removed.

Existing unrelated reliability code is not part of this change. No work is added
solely to protect against a hypothetical failure.
