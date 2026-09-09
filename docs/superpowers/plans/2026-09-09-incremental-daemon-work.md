# Incremental Daemon Work Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task by task. Checkboxes track execution, not approval.

**Goal:** Remove repeated background work while keeping semantic ingestion and
search optional, independent, recoverable, and observable.

**Architecture:** Keep the existing parsers and daemon lifecycle. Add early
source-revision validation, one refresh owner, recoverable persistence, shared
semantic availability, and bounded metrics tailing. Separate independently
testable stages so receiver protocol work does not block local fixes.

**Tech stack:** Go 1.26.5, SQLite, fsnotify, gRPC, typed TOML configuration, and
the existing `livetrack` lifecycle group.

**Spec:** [Approved daemon design](../specs/2026-09-08-incremental-daemon-work-design.md).
The user approved this design on September 9, 2026, after revision `89bf1aa0`.
This plan schedules implementation; unchecked tasks have not been executed.

## Global constraints

- Use the harness-provided isolated checkout. Do not create another worktree or
  include the pre-existing untracked `install` artifact in commits.
- `ingestion_enabled` and `search_enabled` default independently to `false`.
  Reject scoped `enabled`, even when false. Keep no aliases or automatic migration.
- Ingestion disabled does not prevent searching already indexed conversations.
  Both operations require an available engine. Both disabled means zero probes.
- Share one connection attempt and outage cooldown across all consumers. Base
  delays are 30 seconds, one minute, two minutes, four minutes, and five minutes.
  Positive jitter stays within 30 seconds and five minutes. Each attempt retains
  the separate 10-second deadline.
- Provider files and databases remain read-only. No provider writes, migrations,
  triggers, checkpoints, cleanup, or settings changes are permitted.
- Preserve public conversation IDs, message ordering, content selection,
  orphaned-message recovery, deduplication, and ambiguity errors.
- Register long-lived work with `livetrack` before launching it. Drain only through
  the existing group. Preserve listener continuity and one persistence owner.
- Reuse `internal/clock` for wall time. New payloads use concrete named types,
  typed enums, typed maps, and explicit presence where required.
- Run `make check` before each implementation commit. Run the task's targeted
  tests first. Never suppress, baseline, weaken, or delete a failing check.
- Each completed task ends with a signed logical commit and the Codex co-author
  trailer. Stage exact paths, inspect the staged diff, and verify the signature.
- Do not install, deploy, merge, or mutate the production daemon. Isolated live
  tests use the existing harness; manual single-process probes use the sandbox.

## Execution order and completion gates

| Tasks | Deliverable | Dependency |
| --- | --- | --- |
| 0 and 0B | Establish passing baseline checks and durable file replacement. | None |
| 1-3 | Optional semantic configuration, shared availability, and dependency-free validation. | 0B |
| 4-7 | Correct Cursor identity, shared discovery, cancellation, and atomic index generations. | 0B; use 1 for isolated configuration |
| 8-10 | Recoverable metrics storage, incremental input, and migration/retention. | 0B |
| 11-12 | Verify receiver capabilities, then bound and journal semantic delivery. | 2 and 7; protocol gate in 11 |
| 13-14 | Runtime diagnostics and safe profiling with correct listener inheritance. | 1, 2, and published counters from 7/10/12 |
| 15 | Paired performance evidence, complete regression checks, and PR announcement. | Every accepted implementation task |

The discovery, metrics, and diagnostics stages can be reviewed separately from
durable semantic delivery. An unresolved receiver capability prevents declaring
Tasks 11-12 complete; it does not justify omitting other tasks. A narrower active
SQLite row-change path is accepted only after the evidence gate in Task 5.

### Task 0: Establish the baseline without weakening checks

**Modify:** [Anthropic provider](../../../internal/adapter/anthropic/provider.go)
and [dispatch](../../../internal/adapter/anthropic_provider_dispatch.go) only if
the existing formatter findings still reproduce.

**Consumes:** Current checkout and canonical Make targets. **Produces:** A passing
baseline and a separate formatting-only commit when needed.

- [ ] Run `make check`. The last observed failure was `goimports` formatting at
  provider line 142 and dispatch line 246. Reproduce rather than assume it remains.
- [ ] Read both current files. Apply the formatter only to those files, then
  verify the change is formatting-only:

  ```sh
  gofmt -w internal/adapter/anthropic/provider.go internal/adapter/anthropic_provider_dispatch.go
  ```

- [ ] Run `make check` again. Preserve all checks. If a different failure appears,
  diagnose it before attributing it to the implementation.
- [ ] Commit only the verified baseline correction with subject
  `Format Anthropic provider result literals`. Do not include it silently in a
  behavior commit.

### Task 0B: Establish the shared durable-file primitive

**Create:** `internal/durablefile/write.go`, `identity.go`, `write_test.go`, and
`identity_test.go`. Existing authentication and metrics checkpoint writers do
not establish the complete directory-durability contract. Extract compatible
logic rather than adding another concern-specific temporary-file writer.

**Consumes:** Go filesystem APIs. **Produces:** These concrete functions for
cooldown, cache, and metrics checkpoint writers:

```go
type Identity struct {
    Device uint64 `json:"device"`
    Inode uint64 `json:"inode"`
}
type Writer interface {
    Replace(context.Context, string, []byte, fs.FileMode) error
}
```

The package-level `Replace(ctx context.Context, path string, data []byte,
mode fs.FileMode) error` performs production writes. `IdentityOf(file *os.File)
(Identity, error)` derives identity from the open descriptor with platform
filesystem metadata. The interface above is a narrow interruption seam for
storage integration tests, not a separate production implementation.

- [ ] Add real-file tests for creating a file, replacing existing bytes, requested
  permissions, cancellation before replacement, unique temporary files, and
  concurrent readers seeing only one complete version. Assert final contents and
  retained old contents on failure before rename.
- [ ] Implement: validate context; create a unique temporary file in the target
  directory; set mode; write all bytes; sync and close; recheck cancellation;
  rename; sync the containing directory. Clean up an uncommitted temporary file
  and preserve errors. A post-rename durability error is not reported as success.
- [ ] Provide a test-only hook at write, file-sync, rename, and directory-sync
  boundaries, then inject each failure. Do not promise rollback after rename;
  callers retain enough identity/generation state to reconcile that outcome.
- [ ] Add the public-boundary smoke test:

  ```go
  func TestReplacePublishesCompleteContents(t *testing.T) {
      path := filepath.Join(t.TempDir(), "state.json")
      if err := Replace(context.Background(), path, []byte("old\n"), 0o600); err != nil { t.Fatal(err) }
      if err := Replace(context.Background(), path, []byte("new\n"), 0o600); err != nil { t.Fatal(err) }
      got, err := os.ReadFile(path)
      if err != nil { t.Fatal(err) }
      if string(got) != "new\n" { t.Fatalf("contents = %q", got) }
  }
  ```

- [ ] Run `go test -race ./internal/durablefile -count=1` and `make check`.
  Commit as `Add durable replacement for Clyde-owned state files`.

### Task 1: Replace the semantic configuration contract

**Modify:** [Configuration](../../../internal/config/conversation_config.go),
[loader](../../../internal/config/load.go),
[direction tests](../../../internal/config/conversation_semantic_directions_test.go),
[removed-key tests](../../../internal/config/load_removed_keys_test.go),
[example](../../../clyde.example.toml),
[daemon wiring](../../../internal/daemon/run.go),
[sandbox](../../../internal/cli/daemon/sandbox.go),
[live harness](../../../test/live/harness.go),
[live configuration template](../../../test/live/conversation_config.toml.tmpl),
[harness tests](../../../test/live/harness_semantic_config_test.go), and
[update probe](../../../cmd/ci-auto-update/main.go).
Update other typed field initializers found by references, not a global textual
replacement of unrelated `Enabled` fields.

**Create:** `internal/config/conversation_decode.go` and
`internal/config/conversation_load_test.go`.

**Consumes:** Existing configuration loading. **Produces:** The existing
`FeedsEngine() bool`, `AnswersSearch() bool`, and `UsesEngine() bool` methods with
new independent defaults. Preserve presence for runtime provenance:

```go
type SemanticDirections struct {
    Ingestion bool
    Search bool
    IngestionExplicit bool
    SearchExplicit bool
}

func semanticFlagEnabled(value *bool) bool {
    return value != nil && *value
}
```

- [ ] Add these real-loader cases before modifying the defaults. Keep TOML input
  in `internal/config/testdata/semantic/*.toml`; case names select fixtures.
  Missing configuration also exercises `LoadGlobalOrDefault` with a temporary
  configuration root. The existing default-on tests must be rewritten around
  explicit search opt-in, preserving their search-without-ingestion assertion.

  ```go
  func TestSemanticConfigLoadsIndependentDirections(t *testing.T) {
      cases := []struct {
          name string
          ingestion bool
          search bool
      }{
          {"omitted", false, false},
          {"both_false", false, false},
          {"ingestion_only", true, false},
          {"search_only", false, true},
          {"both_true", true, true},
      }
      for _, testCase := range cases {
          t.Run(testCase.name, func(t *testing.T) {
              fixture := filepath.Join("testdata", "semantic", testCase.name+".toml")
              body, err := os.ReadFile(fixture)
              if err != nil { t.Fatal(err) }
              directory := t.TempDir()
              if err := os.WriteFile(filepath.Join(directory, "config.toml"), body, 0o600); err != nil { t.Fatal(err) }
              cfg, err := loadConfig(directory)
              if err != nil { t.Fatal(err) }
              semantic := cfg.Conversation.Semantic
              if semantic.FeedsEngine() != testCase.ingestion || semantic.AnswersSearch() != testCase.search {
                  t.Fatalf("directions: ingestion=%v search=%v", semantic.FeedsEngine(), semantic.AnswersSearch())
              }
              if semantic.UsesEngine() != (testCase.ingestion || testCase.search) {
                  t.Fatal("engine use disagrees with enabled directions")
              }
          })
      }
  }
  ```

  Fixture content is exact: `omitted` contains only `[conversation]`;
  `both_false` sets both new flags false; `ingestion_only` sets only
  `ingestion_enabled = true`; `search_only` sets only `search_enabled = true`;
  `both_true` sets both true under `[conversation.semantic]`.

- [ ] Run `go test ./internal/config -run TestSemanticConfigLoadsIndependentDirections -count=1`.
  Expect the omitted and ingestion-only cases to expose current default-on search.
- [ ] Replace `Enabled` with `IngestionEnabled *bool`; keep `SearchEnabled *bool`
  so explicit false remains distinguishable from omission. Use TOML names
  `ingestion_enabled`/`search_enabled` and JSON names
  `ingestionEnabled`/`searchEnabled`. Implement:

  ```go
  func (semantic ConversationSemanticConfig) FeedsEngine() bool {
      return semanticFlagEnabled(semantic.IngestionEnabled)
  }
  func (semantic ConversationSemanticConfig) AnswersSearch() bool {
      return semanticFlagEnabled(semantic.SearchEnabled)
  }
  func (semantic ConversationSemanticConfig) Directions() SemanticDirections {
      return SemanticDirections{
          Ingestion: semantic.FeedsEngine(),
          Search: semantic.AnswersSearch(),
          IngestionExplicit: semantic.IngestionEnabled != nil,
          SearchExplicit: semantic.SearchEnabled != nil,
      }
  }
  ```

- [ ] Reject `conversation.semantic.enabled`, `sync_to_engine`, and `query_engine`
  before normal decoding can ignore them. Extend the existing typed removed-key
  inspection, but return errors for these keys. Presence, including explicit
  false, must be detected. Invalid types also fail. Where JSON configuration is
  accepted, reject scoped `enabled`, `syncToEngine`, and `queryEngine`; ordinary
  `searchEnabled` remains valid. Do not add a new JSON input surface.
- [ ] Add fixtures for false old keys, mixed old/new keys, withdrawn aliases,
  invalid types, explicit false provenance, and unrelated adapter `enabled`.
  Assert rejected key and replacement in the error. For reload rejection, assert
  the old generation remains identified as active.
- [ ] Update all producers to emit new keys explicitly where they intend opt-in.
  Add sandbox flags `--ingestion-enabled` and `--search-enabled`, both false;
  ordinary sandbox startup must not contact the engine. Preserve the sandbox's
  isolated roots and single-process lifetime.
- [ ] Run `go test ./internal/config ./internal/cli/daemon ./internal/daemon`,
  the existing tagged harness configuration tests, and `make check`. Commit as
  `Replace semantic enabled with independent ingestion and search flags`.

### Task 2: Share availability, retry timing, and failure transitions

**Modify:** [Semantic runtime](../../../internal/daemon/conversation_semantic_runtime.go),
[engine client](../../../internal/conversation/semsearch/client.go),
[search source](../../../internal/daemon/conversation_search_source.go),
[search errors](../../../internal/daemon/conversation_search_errors.go),
[sync worker](../../../internal/daemon/conversation_semantic_sync.go), and
[recovery tests](../../../internal/daemon/conversation_semantic_recovery_test.go).

**Create:** `internal/daemon/conversation_semantic_availability.go`,
`internal/daemon/conversation_semantic_availability_test.go`, and
`internal/daemon/conversation_semantic_cooldown.go`.

**Consumes:** Task 1 directions, existing connector and lifecycle group.
**Produces:** A pure status snapshot and the sole connection-attempt admission
path. Keep these package-private until Task 13 maps them to public status:

```go
type semanticAvailabilityState string
const (
    semanticDisabled semanticAvailabilityState = "disabled"
    semanticConnecting semanticAvailabilityState = "connecting"
    semanticReady semanticAvailabilityState = "ready"
    semanticUnavailable semanticAvailabilityState = "unavailable"
)
type semanticAvailabilitySnapshot struct {
    State semanticAvailabilityState
    Directions config.SemanticDirections
    Attempts uint64
    ConsecutiveFailures uint32
    NextAttemptAt time.Time
    LastFailureClass string
}
type semanticCooldown struct {
    Version uint32
    ConfigurationFingerprint string
    ConsecutiveFailures uint32
    NextAttemptAt time.Time
}
type semanticRetryTimer interface {
    C() <-chan time.Time
    Stop() bool
}
type semanticRetryClock interface {
    Now() time.Time
    NewTimer(time.Duration) semanticRetryTimer
}
```

Add `(r *conversationSemanticRuntime) snapshotAvailability()
semanticAvailabilitySnapshot` for passive reads and
`(r *conversationSemanticRuntime) reportTransportFailure(ctx context.Context,
cause error)` for client wrappers to invalidate a failed transport. The latter
classifies the cause and changes admission state; it never dials. Runtime
construction receives one `semanticRetryClock`, with production wall reads
delegating to `clock.Now` and a real timer wrapper. Tests supply a local clock.

- [ ] Extend `newTestSemanticRuntime`, `flakySemanticConnector`, and
  `blockingSemanticConnector` to exercise one runtime, not separate feeder and
  query retry loops. Keep the existing cancellation and recovery tests. Add a
  real Unix-socket gRPC fixture for physical connection counts, including failed
  handshakes and a receiver that appears after cooldown.
- [ ] Introduce a deterministic scheduling seam: the runtime owns a clock-based
  `nextAttemptAt` and one timer. Tests advance a local injected clock/timer;
  production delegates wall reads to `clock.Now`. Do not mutate the package-wide
  clock concurrently in parallel tests. Test exact base delays with zero jitter:

  ```go
  func semanticRetryDelay(failures uint32, jitter time.Duration) time.Duration {
      base := 30 * time.Second
      for remaining := failures; remaining > 1 && base < 5*time.Minute; remaining-- {
          base = min(2*base, 5*time.Minute)
      }
      return min(base+max(jitter, 0), 5*time.Minute)
  }
  ```

  `failures` is at least one after an unsuccessful attempt. Generate production
  jitter in `[0, base/5]`; clipping to the cap is intentional. Test attempts at
  the deadline and one tick before it, concurrent queries, socket-event bursts,
  and unchanged-configuration reload. No trigger may advance `nextAttemptAt`.
- [ ] Change startup to construct the runtime only for `UsesEngine()`, register
  its stop hook first, and attempt registration asynchronously. Retain
  `attemptMu` as single-flight protection. The worker admits attempts only at or
  after the stored deadline. On failure, close the failed transport and persist
  the updated cooldown before scheduling another attempt.
- [ ] Replace the existing registered-forever assumption with typed transport
  failure feedback. Query and feeder wrappers report transport loss to the same
  runtime, clear access to the failed client, and cancel/close its connection.
  Permission, invalid-request, and collection-policy errors must not trigger a
  reconnect storm. During cooldown, search returns the existing caller-safe
  unavailable category immediately and feeding returns before index reads.
- [ ] Inspect the pinned engine client's `DialDaemon` implementation. Count its
  physical dials in the socket fixture. Route its context dialer through the
  shared admission budget or close failed connections before they can reconnect
  independently. Do not assume gRPC's own retries obey the outer worker's timer.
- [ ] Persist only cooldown transitions, using the durable replacement primitive
  established in Task 0B.
  Key state by resolved socket, collection, and effective directions; load it
  only after opt-in. A malformed checkpoint logs once and starts a conservative
  cooldown. An unchanged-config reload preserves the deadline. Disabling both
  flags creates no timer or dependency probe and retains delivery records.
- [ ] Make one runtime boundary log unavailable/recovered transitions. Remove
  duplicate helper warnings for errors already returned to that boundary.
  Record sanitized failure class, attempt count, and deadline. A successful
  registration or other successful engine operation establishes readiness;
  merely creating a lazy gRPC client does not.
- [ ] Add `conversation_search_disabled` to the existing typed search error
  boundary. Keep disabled and unavailable distinguishable without changing
  unavailable into the CLI's misleading "daemon is not running" path.
- [ ] Run `go test -race ./internal/daemon -run 'Semantic|SearchConversations' -count=1`,
  `go test ./internal/conversation/semsearch`, and `make check`. Commit as
  `Share semantic availability and persist outage cooldowns`.

### Task 3: Prove clients can operate without the engine

**Modify:** The existing live harness and sandbox tests from Task 1.
**Create:** `test/live/semantic_optional_live_test.go` and
`internal/daemon/conversation_semantic_optional_test.go`.

**Consumes:** Real configuration loader, runtime from Task 2, raw conversation
fixtures, and existing isolated daemon harness. **Produces:** Public-boundary
evidence for missing dependency, independent directions, and recovery.

- [ ] Add table-driven daemon scenarios for no configuration file, omitted
  semantic section, both false, ingestion only, search only, and both true.
  Test omitted settings by actual omission; do not let the harness write them.
  Run raw listing, retrieval, context, export, repeated status, and disabled
  semantic requests against each disabled case.
- [ ] Add an instrumented engine fixture with one stored conversation. In
  search-only mode, assert the returned conversation text and zero manifest or
  upsert requests. In ingestion-only mode, assert delivered documents and a typed
  disabled search error. Reuse the existing test engine protocol types.
- [ ] Record attempted socket resolutions/dials as well as accepted connections.
  Observe disabled cases for at least five minutes in the live suite. They must
  record zero probes, retry wakes, registrations, and feeder passes even when a
  sentinel engine socket exists. Keep accelerated deterministic tests separate.
- [ ] Hold an opted-in attempt in flight, apply both flags false through the
  real reload boundary, and assert cancellation before applied status. Re-enable
  and assert exactly one runtime. Test configuration rejection with removed keys
  without claiming that rejection disabled the previous configuration.
- [ ] Run the targeted live cases through the harness and `make check`. Commit
  as `Verify semantic opt-in and dependency-free daemon operation`.

### Task 4: Preserve remote workspace identity

**Modify:** [Cursor paths](../../../internal/providers/cursor/store/paths.go),
[registry](../../../internal/providers/cursor/store/registry.go), and the existing
workspace descriptor tests located beside them.
**Create:** `internal/providers/cursor/store/workspace_location_test.go`.

**Consumes:** Current descriptor decoder. **Produces:** Correct remote identities
through the existing descriptor boundary, without changing conversation IDs.

- [ ] Reproduce the encoded-authority defect through `ReadWorkspaceFolderPath`,
  not through a second standalone URI parser:

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

- [ ] Run `go test ./internal/providers/cursor/store -run TestRemoteWorkspaceAuthoritySurvivesDescriptorRead -count=1`.
  Expect the current invalid URL escape failure.
- [ ] Extend the existing URI boundary to classify the scheme before local-file
  host decoding. Represent local, remote, absent, unsupported, and invalid
  outcomes with an explicit kind and payload. Preserve remote scheme/authority
  text; never decode it into a local filesystem path. Keep local escaped spaces,
  localhost, platform-specific drive paths, empty windows, and malformed input
  behavior covered through the descriptor API.
- [ ] Move repeated descriptor diagnostics to the store snapshot boundary in
  Task 5. Until that cache exists, log one failure at the registry boundary and
  return typed causes from lower helpers. Update workspace consumers to avoid
  sending remote identities to filesystem operations.
- [ ] Test a readable workspace database with an invalid descriptor: its
  conversations remain visible with incomplete workspace metadata. Run the
  complete Cursor store/parser tests and `make check`. Commit as
  `Preserve encoded remote Cursor workspace identities`.

### Task 5: Cache shared discovery before expensive SQLite reads

**Modify:** [Cursor parser](../../../internal/providers/cursor/parser/parser.go),
[composer scan](../../../internal/providers/cursor/parser/composer_scan.go),
[bubble stock](../../../internal/providers/cursor/store/stock.go),
[request lookup](../../../internal/providers/cursor/store/request.go), and the
store registry from Task 4.
**Create:** `internal/providers/cursor/store/discovery_snapshot.go`,
`internal/providers/cursor/store/discovery_snapshot_test.go`, and
`internal/providers/cursor/store/store_revision.go`.

**Consumes:** Existing typed store readers and their metadata merge rules.
**Produces:** One shared inventory and per-source snapshot, used by composer,
legacy, and request lookup. The revision token is opaque to generic indexing;
provider validation owns its meaning.

```go
type StoreRevision struct {
    DatabaseIdentity string
    DatabaseSize int64
    DatabaseModifiedAt time.Time
    WALIdentity string
    WALSize int64
    WALModifiedAt time.Time
    DescriptorDigest string
    ReaderVersion uint32
}
type DiscoveryOutcome uint8
const (
    DiscoveryComplete DiscoveryOutcome = iota
    DiscoveryIncomplete
    DiscoveryAbsent
)
```

- [ ] Use real temporary SQLite stores to measure queries and opened content
  stores through the public discovery path. Run composer and legacy discovery
  together, then request lookup. Assert identical conversations and one shared
  metadata snapshot, rather than separate inventory walks and database passes.
- [ ] Prove revision validation before trusting the new cache. Write with a
  separate SQLite connection and test WAL-only commits, checkpoint/reset,
  historical updates, deletion, replacement, and same-size writes. A retained
  connection's `data_version` is connection-local; never persist or compare it
  after reconnect. Metadata plus event evidence must detect every fixture change.
  If unchanged status cannot be established, reconcile rather than guessing.
- [ ] Test more workspaces than the connection budget. Connection eviction must
  not force every idle reconciliation to reread content. This is a feasibility
  gate: if the token is insufficient, improve the token or retained-connection
  strategy before marking the task complete. Do not weaken the zero-content-work
  assertion to make an ineffective cache pass.
- [ ] Cache successful, empty, and failed outcomes separately. One changed store
  invalidates its contribution. Incomplete reads retain the prior contribution
  and report stale coverage. Only successful parent discovery can prove deletion.
  Key failure suppression by source revision and failure class; schedule transient
  access failures for retry even when content metadata did not change.
- [ ] Reconcile a changed global store in one consistent pass grouped by composer,
  preserving the existing bubble-stock revision and metadata merge semantics.
  Do not replace orphaned-message discovery with the composer's reference list.
  Publish only changed contributions, and retain events received during the pass.
- [ ] Benchmark an active global store separately. If the provider offers no
  trustworthy row-change signal, retain the full dirty-store pass and report its
  measured cost. A future delta reader requires recorded write evidence and an
  equivalence test against full reconciliation for edits and deletions.
- [ ] Run `go test -race ./internal/providers/cursor/store ./internal/providers/cursor/parser -count=1`
  and `make check`. Commit as `Share revision-aware Cursor discovery snapshots`.

### Task 6: Carry cancellation and append progress through existing parsers

**Modify:** [Parser contracts](../../../internal/conversation/parser.go),
[generic scan](../../../internal/conversation/scan.go),
[Cursor resume links](../../../internal/providers/cursor/parser/resume_links.go),
[Cursor transcript decoder](../../../internal/providers/cursor/jsonl/transcript.go),
[Copilot complete-line reader](../../../internal/providers/copilot/parser/parser.go),
and every registered implementation of the changed parser methods.
**Create:** `internal/conversation/source_progress.go` and
`internal/conversation/source_progress_test.go`.

**Consumes:** Existing `FileStamp{Size, Mtime}`, multi-conversation complete offsets,
and provider-owned parsing state. **Produces:** Context-aware public parser calls
and one source-progress contract; update callers and implementations together:

```go
type SourceKey struct {
    Provider Provider
    Path string
}
type SourceProgress struct {
    Identity durablefile.Identity
    Stamp FileStamp
    CompleteOffset int64
    PrefixDigest string
    ParserVersion uint32
}
type ContextParser interface {
    Provider() Provider
    Discover(context.Context, map[string]Record) ([]ScanCandidate, error)
    ScanRecord(context.Context, string, FileStamp) (Record, bool)
    Stream(context.Context, string, LoadOptions) iter.Seq2[transcript.Message, error]
}
```

Use the existing `Parser` name for the final interface; `ContextParser` above
illustrates the replacement contract, not a second permanent parser hierarchy.
Add `context.Context` to `ScanRecords` and `StreamSelected` as well. Keep
provider continuation DTOs in provider packages and persist them with that
provider's versioned snapshot; do not import Cursor types into `conversation`.

- [ ] Extend `scan_test.go` and `multi_scan_cache_test.go` around the existing
  appended-byte, truncation, failed-read, and persisted-offset cases. Reuse
  `TestResumeLinksAreNotRereadWhenAParentIsUnchanged` and
  `TestScanRecordsDiscoversSubagentFromAppendedBytes` as end-to-end controls.
- [ ] Extract the generic complete-line byte boundary from `readCompleteEvents`
  rather than copying another newline scanner. Its result distinguishes a
  complete consumed prefix from an incomplete final line and cancellation.
  Provider decoders continue to own event interpretation.
- [ ] Feed resume-link extraction from the same decoded appended records used
  for conversation scanning. Persist all required continuation, including the
  open turn; a checkpoint that advances past an unpersisted open turn is invalid.
  Remove the independent changed-parent scan from byte zero after equivalence
  tests pass.
- [ ] Validate source identity and consumed-prefix continuity on resume. A
  replacement, truncation, invalid prefix, or parser version mismatch resets only
  that source. Do not claim that size and mtime prove append-only modification.
  Revalidation cost is recorded separately from new-content parsing.
- [ ] Add a cancellation test that parks a real reader between complete records,
  cancels through the public scan context, and asserts no later record is
  published and the prior durable offset remains usable. Test restart with a
  split final line, appended subagent link, and in-place edit before the offset.
- [ ] Run `go test -race ./internal/conversation ./internal/providers/... -count=1`
  and `make check`. Commit as `Unify cancellable source progress and resume-link parsing`.

### Task 7: Publish index generations through one lifecycle owner

**Modify:** [Index](../../../internal/conversation/index.go), generic scan and
parser contracts from Task 6, daemon startup from Task 1,
[runtime](../../../internal/daemon/runtime.go), and
[reload](../../../internal/daemon/reload.go).
**Create:** `internal/conversation/discovery.go`, `scheduler.go`, `snapshot.go`,
`cache.go`, their matching test files, and
`internal/daemon/conversation_scheduler.go`.

**Consumes:** Source progress, provider snapshots, durable replacement, and the
daemon's existing process-lock ownership. **Produces:** Immutable published
snapshots and validated freshness barriers. Extend the current refresh owner;
do not leave the old periodic scanner running beside it.

```go
type Generation struct {
    Epoch string `json:"epoch"`
    Sequence uint64 `json:"sequence"`
}
type RefreshReason string
const (
    RefreshStartup RefreshReason = "startup"
    RefreshSourceChanged RefreshReason = "source_changed"
    RefreshExplicit RefreshReason = "explicit"
    RefreshReconcile RefreshReason = "reconcile"
)
type FreshnessRequest struct {
    Sources []SourceKey
    Providers []Provider
    Reason RefreshReason
}
type SourceCoverage struct {
    Source SourceKey
    Complete bool
    ConfirmedAbsent bool
    FailureClass string
}
type Snapshot struct {
    Generation Generation
    Records []StampedRecord
    Coverage []SourceCoverage
}
type RefreshIndex interface {
    Snapshot() Snapshot
    RequestRefresh(context.Context, FreshnessRequest) (Generation, error)
}
```

`Index` implements `RefreshIndex`. Returned slices must not expose mutable index
storage. Empty sources with explicit providers means reconcile those providers;
both empty means reconcile all registered providers. Only successfully reconciled
parent coverage can authorize removal of a prior contribution.

- [ ] Extend `request_index_test.go`: an unknown request introduced after the
  cached snapshot must be found, duplicates remain ambiguous, simultaneous
  requests share one reconciliation, and failed refresh remains an error. Add
  a missed-event case so an empty queue cannot satisfy freshness on its own.
- [ ] Replace cached `List` and `ListWithStamps` refresh triggers with pure
  snapshot reads. Keep explicit `Refresh` as a compatibility wrapper over
  `RequestRefresh`; it validates source revisions after admission. It captures
  its barrier once, so continuous later appends cannot postpone it forever.
- [ ] Install watchers before startup reconciliation. Merge dirty source keys;
  retain arrivals during a pass. Bound the queue and collapse overflow to dirty
  provider roots. Use round-robin provider service and bounded work units so an
  active global store cannot starve a small transcript.
- [ ] Register scheduler, watchers, retained connections, and worker completion
  with `livetrack` before starting them. Remove `context.WithoutCancel` and the
  bare index goroutine. Preserve the one-minute lightweight reconciliation
  cadence and make explicit freshness bypass coalescing.
- [ ] Extract the version-4 cache logic into the cache unit and add versioned
  epoch/generation, coverage, and progress. Load old cache for fast startup but
  reconcile before trusting missing revision metadata. Compare records and
  durable progress before encoding; unchanged observation passes write nothing.
  Use `durablefile.Replace` for changed snapshots and leave work pending if
  persistence fails.
- [ ] Build reload ownership into the existing daemon process-lock lifecycle.
  The child serves a completed cache and can report readiness before acquiring
  write ownership. Only after old workers stop and ownership transfers does the
  child reread the final committed cache and start mutation. Do not use inherited
  file-descriptor locks as if parent and child held independent leases.
- [ ] Test a real cache reader racing repeated atomic publication. After every
  injected write boundary, it reads a complete old or new generation. Add reload
  during scan, watcher overflow, event-during-publication, callback cancellation,
  and startup cache corruption cases through the existing live harness.
- [ ] Run `go test -race ./internal/conversation ./internal/daemon -count=1`,
  targeted isolated reload tests, and `make check`. Commit as
  `Own incremental index refresh and atomic generations in the daemon lifecycle`.

### Task 8: Commit metrics output and checkpoints as one recoverable state

**Modify:** [Metrics history](../../../internal/daemon/metrics_history.go),
[rollup](../../../internal/daemon/metrics_rollup.go),
[report reader](../../../internal/daemon/metrics_rollup_report.go),
[worker](../../../internal/daemon/metrics_rollup_worker.go), and their tests.
**Create:** `internal/daemon/metrics_rollup_transaction.go`,
`metrics_rollup_checkpoint.go`, and `metrics_rollup_transaction_test.go`.

**Consumes:** Existing event aggregation, canonical rollup lock, and Task 0B.
**Produces:** A versioned checkpoint and committed-boundary readers. Change
writers, readers, and generation markers in the same task; pairing a new writer
with an unlocked old reader is unsafe.

```go
type metricsSourceCursor struct {
    Identity durablefile.Identity
    Path string
    CompleteOffset int64
    Closed bool
    Compressed bool
}
type metricsCommittedOutput struct {
    Generation string
    Identity durablefile.Identity
    CommittedLength int64
}
type metricsPendingRequest struct {
    ExecutionID string
    Started bool
    Terminal bool
    LifecycleStarted bool
    LifecycleTerminal bool
    InvalidLifecycle bool
    Status metricsRequestStatus
    TotalMS int64
    BytesIn int64
    BytesOut int64
    PromptTokens int64
    OutputTokens int64
    CacheTokens int64
    CacheReadTokens int64
    CacheCreationTokens int64
    Model string
    IOSeen bool
    StartedAt time.Time
    StreamedAt time.Time
    TerminalAt time.Time
    LegDurations map[string]int64
}
type metricsCoverageState struct {
    EarliestObserved time.Time
    ObservedThrough time.Time
    IncompleteReasons []string
}
type metricsRollupCheckpoint struct {
    Version uint32
    Output metricsCommittedOutput
    Sources []metricsSourceCursor
    Pending []metricsPendingRequest
    Coverage metricsCoverageState
    LastPassAt time.Time
    NextPruneAt time.Time
}
```

Use explicit enum types for stored generation/coverage classifications during
implementation. Preserve the current `metricsRequestStatus`. Conversion between
`metricsRequest` and `metricsPendingRequest` is lossless and copies the map;
do not persist a completed `metricsRollupRecord` as unfinished state.

- [ ] Reuse `rollupTestTime`, `writeRollupFixture`, `requestRollupFixture`, and
  `TestRollupRoundTripPreservesAggregationInputs`. Add an unfinished request with
  lifecycle flags, stream time, and leg durations, restart, complete it, and compare
  public report totals with direct log replay.
- [ ] Add typed `metricsStorePaths{RollupPath, CheckpointPath string}`,
  `metricsRollupTransaction`, and `metricsRollupSnapshot`. The transaction holds
  the existing exclusive lock keyed by the canonical legacy path. A snapshot
  holds its shared counterpart, checkpoint, open output descriptor, and an
  `io.SectionReader` bounded by `CommittedLength` until `Close() error`.
  Neither lock key changes when retention selects a new generation.
- [ ] Expose these package-private entry points, used by later tasks:

  ```go
  type metricsCommitter interface {
      Commit(context.Context, []metricsRollupRecord, metricsRollupCheckpoint) error
      Close() error
  }
  ```

  `openMetricsRollupTransaction(ctx context.Context, paths metricsStorePaths)
  (*metricsRollupTransaction, error)` and
  `openMetricsRollupSnapshot(ctx context.Context, paths metricsStorePaths)
  (*metricsRollupSnapshot, error)` implement writer/reader admission.

- [ ] Implement commit in order: validate selected output identity/length;
  discard only a proven uncommitted tail; append complete output records; sync
  output; durably replace the checkpoint. Readers never read beyond the selected
  committed length. If identity mismatches, return incomplete/corrupt state and
  do not truncate. Generation markers use this same transaction.
- [ ] Inject interruption before append, after append, after file sync, after
  checkpoint rename, and after directory sync. Reopen through
  `metricsWindowsFromRollupPath` after each interruption. Require exact totals
  without duplicate visible records, preserved pending state, and safe replay.
  Add concurrent reader/retention lock lifetime tests.
- [ ] Run `go test -race ./internal/daemon -run 'Rollup|MetricsHistory' -count=1`
  and `make check`. Commit as `Make metrics checkpoints and output recoverable`.

### Task 9: Tail complete metrics records from durable byte cursors

**Modify:** Metrics history and worker from Task 8, plus
[distiller](../../../internal/daemon/metrics_rollup_distill.go).
**Create:** `internal/daemon/metrics_source_reader.go` and
`internal/daemon/metrics_source_reader_test.go`.

**Consumes:** Committed cursors, pending aggregates, and the existing metrics
decoder/aggregator. **Produces:** Bounded input batches and exact resumable state.

```go
type metricsReadLimits struct {
    MaxBytes int64
    MaxRecords int
}
type metricsLineKind uint8
const (
    metricsLineEvent metricsLineKind = iota
    metricsLineOther
    metricsLineInvalid
)
type metricsDecodedLine struct {
    Kind metricsLineKind
    Record metricsLogRecord
    RecordedAt time.Time
}
type metricsSourceBatch struct {
    Lines []metricsDecodedLine
    Next metricsSourceCursor
    BytesRead int64
    AtEOF bool
}
```

- [ ] Extract `decodeMetricsHistoryLine(line []byte) (metricsDecodedLine, error)`
  from the existing historical reader and use it in both paths. Preserve parsing,
  coverage, and request identity semantics. `ExecutionID` remains the key when
  present, with `RequestID` as fallback.
- [ ] Implement `readMetricsSourceBatch(ctx context.Context, file *os.File,
  cursor metricsSourceCursor, limits metricsReadLimits) (metricsSourceBatch, error)`.
  Seek to the committed offset, consume only complete lines, and check
  cancellation within the loop. Non-metric lines advance progress. Invalid
  complete lines advance with explicit incomplete coverage. An incomplete final
  line does not advance. An oversized line is an explicit bounded failure, not
  silent truncation or an infinite loop at the same position.
- [ ] Extend `twoRequestLog` and `writeMetricsHistoryRecords` fixtures to append
  one event at a time, split a line, and complete one request across rotations.
  Compare each committed report with `BuildMetricsHistory` on the same bytes.
  Add requests sharing a request ID but having different execution IDs.
- [ ] Persist pending request state after each bounded batch in the same commit
  as source offsets and output. Bound resident pending aggregates; spill complete
  typed state to Clyde-owned storage when necessary, or return an explicit
  resource/coverage result without advancing past data that cannot be retained.
- [ ] Test zero new bytes with no retention expiry: no history content read,
  output append, or checkpoint rewrite. Use filesystem byte counters through the
  reader boundary, not mtime alone. Preserve `TestDistillIsIdempotentAcrossPasses`,
  cancellation, lock-wait, and report-equivalence tests.
- [ ] Run the complete metrics test selection and `make check`. Commit as
  `Resume metrics aggregation from complete-line source offsets`.

### Task 10: Migrate metrics history and retain immutable generations

**Modify:** Transaction/checkpoint units from Task 8 and distiller from Task 9.
**Create:** `internal/daemon/metrics_rollup_migration.go`,
`metrics_rollup_retention.go`, and matching tests.

**Consumes:** Legacy output, timestamp checkpoint, retained canonical logs, and
the new transaction. **Produces:** One committed migrated generation and
retention that cannot destroy its previous checkpoint/output pair.

```go
type metricsMigrationInput struct {
    LogPath string
    LegacyOutputPath string
    LegacyCheckpointPath string
    Now time.Time
}
type metricsRetentionResult struct {
    Pruned int
    NextPruneAt time.Time
    Output metricsCommittedOutput
}
```

- [ ] Distinguish missing, legacy, valid-current, and corrupt checkpoints. A
  decode failure must not become a zero checkpoint that silently starts at EOF.
  Migration runs under the canonical exclusive lock and is restartable.
- [ ] Import legacy completed output, reconcile available retained input once,
  reconstruct pending requests, and deduplicate by execution/generation identity.
  Publish new output and checkpoint together. Missing required input marks
  coverage incomplete. Preserve raw provider files and legacy recoverable state
  until the replacement commit is durable.
- [ ] Implement `retainMetricsRollup(ctx context.Context,
  transaction *metricsRollupTransaction, cutoff time.Time)
  (metricsRetentionResult, error)`. When nothing expires, return without rewriting.
  Otherwise write and sync a uniquely named generation, then atomically select it
  in the checkpoint. Delete the previous generation only after durable selection
  and reader release. Keep the current eight-day retention and boundary semantics.
- [ ] Reuse `writeGzipMetricsHistoryRecords` for closed rotations. Test a crash
  after new output creation, after checkpoint selection, and during old-generation
  cleanup. Reopen the real report reader and require exactly the retained totals.
  Keep unparsable retained records and their coverage warnings; do not erase them
  to make the new store look complete.
- [ ] Run `go test -race ./internal/daemon -run 'Rollup|MetricsHistory|Distill' -count=1`
  and `make check`. Commit as `Migrate and retain metrics generations without replaying history`.

### Task 11: Establish the receiver contract before durable delivery

**Inspect and modify only after proof:** [Engine dependency](../../../go.mod),
the semantic client from Task 2, its tests,
and the receiver's public protocol through the pinned dependency.
**Create:** `internal/conversation/semsearch/delivery_contract.go` and
`internal/conversation/semsearch/delivery_contract_test.go`.

**Consumes:** The real receiver protocol. **Produces:** A verified contract for
collection epochs, acceptance recovery, and bounded complete-conversation
delivery. The inspected upsert header has no client delivery ID, and the
inspected collection-registration response exposes no index epoch. Do not infer
those capabilities from an accepted job ID or stable collection name.

```go
type DeliveryCapabilities struct {
    CollectionEpoch bool
    RecoverAcceptanceByDeliveryID bool
    AtomicConversationChunks bool
    BoundedManifestPaging bool
}
func (capabilities DeliveryCapabilities) SupportsDurableDelivery() bool {
    return capabilities.CollectionEpoch &&
        capabilities.RecoverAcceptanceByDeliveryID &&
        capabilities.AtomicConversationChunks &&
        capabilities.BoundedManifestPaging
}
```

- [ ] Inspect the complete receiver service, registration, manifest, upsert,
  and job-state contracts. Record actual field and method names in the contract
  tests. Existing framing caps of 1,000 documents and 3 MiB do not prove bounded
  preparation: current callers have already allocated the full slice, and the
  manifest is sent as one message.
- [ ] Build an isolated receiver fixture that accepts a delivery but drops the
  response. Reconcile by the submitted delivery identity and assert one accepted
  job. Rebuild the collection and prove that its epoch changes, invalidating
  acknowledgements. Test oversized conversations and manifests without deleting
  earlier chunks or silently truncating content.
- [ ] If the existing protocol supports these semantics through different
  primitives, implement a typed adapter and prove it with those tests. If not,
  specify the exact receiver extension and its compatibility version before
  changing the dependency. An extension requires its own reviewed change in the
  receiver project; do not invent protobuf fields in Clyde or report Task 12 done.
- [ ] Keep unsupported/unknown capability as a typed error. Do not return true
  from a stub, equate transport streaming with transactional replacement, or
  suppress needed revisions to make the queue look drained. Local-only tasks
  remain executable while this gate is unresolved.
- [ ] Run `go test ./internal/conversation/semsearch -run DeliveryContract -count=1`
  against the isolated receiver plus `make check`. Commit a working verified
  adapter as `Define the semantic delivery capability contract`; otherwise retain
  evidence and leave this task unchecked with the exact missing capability.

### Task 12: Bound projection and journal accepted semantic work

**Modify:** Sync worker and semantic client from Tasks 2/11,
[document projection](../../../internal/daemon/conversation_semantic_documents.go),
and existing memo, pinning, generation, content-policy, and suppression tests.
**Create:** `internal/daemon/conversation_semantic_delivery.go`,
`conversation_semantic_projection_stream.go`, and matching tests.

**Consumes:** Verified receiver capabilities, Task 2 availability, Task 7
snapshots, context-aware message streams, and Task 0B persistence.
**Produces:** Bounded preparation and durable delivery state without weakening
the engine's needed-revision authority.

```go
type semanticDeliveryState string
const (
    deliveryPrepared semanticDeliveryState = "prepared"
    deliveryAccepted semanticDeliveryState = "accepted"
    deliveryUnknown semanticDeliveryState = "unknown"
    deliveryCompleted semanticDeliveryState = "completed"
    deliveryFailed semanticDeliveryState = "failed"
    deliveryCancelled semanticDeliveryState = "cancelled"
)
type semanticDeliveryRecord struct {
    Version uint32
    DeliveryID string
    CollectionID string
    CollectionEpoch string
    ProjectionVersion uint32
    Revisions []semsearch.Fingerprint
    State semanticDeliveryState
    JobID string
    PayloadPath string
    PayloadDigest string
    SubmittedDocuments uint64
    SubmittedBytes uint64
}
```

- [ ] Change `engineBusyWithLastJob` so failed, missing, empty, or unknown job
  lookups retain unknown acceptance state and stop resubmission. Only explicit
  terminal outcomes release the outstanding delivery. Keep the availability
  gate separate from asynchronous job state.
- [ ] Extend existing failed-load, empty-document, pinning, and content-policy
  tests through `runPass`. Use a fixture that returns the same needed IDs after
  accepted-but-unfinished work, then completes it. Assert zero duplicate delivery
  and restored progress after restart. Test collection rebuild separately so old
  acknowledgements cannot hide newly needed content.
- [ ] Replace whole-needed-set `collectNeededDocuments` materialization with a
  bounded message projection iterator. Reuse the existing content selector and
  `BuildSemanticConversationDocuments` projection logic one message at a time,
  preserving original message indexes and stripping tallies. Spool oversized
  complete conversations into private Clyde-owned files, tracking byte length and
  digest, so memory limits do not become permanent deferral.
- [ ] Use one canonical projected-content digest. Extend
  `SemanticProjectionHash` to cover every receiver-visible field: conversation
  and parent IDs, message index, role, timestamp, text, thinking, all tool fields,
  workspace, archive state, and loading rules. Include projection version. Add
  public feeder tests for metadata-only updates and unchanged-content stamp moves;
  only genuinely unchanged projections may reuse prior prepared content.
- [ ] Persist `prepared` with delivery identity and payload digest before send;
  persist `accepted` with the receiver's job ID after acceptance; record
  `unknown` on a lost response; advance acknowledged revisions only after
  `completed`. Reconcile disk state with receiver epoch and jobs after restart.
  Never drop incomplete state solely because the local process changed.
- [ ] Frame documents and manifest pages through the receiver contract verified
  in Task 11. Limits apply before full allocation and before every wire message.
  Preserve complete-conversation replacement and retention of omitted
  conversations. A single large message must use the supported lossless protocol
  path or leave this task unaccepted; do not silently strip fields to meet a cap.
- [ ] Publish bounded counts/digests for requested, prepared, submitted, accepted,
  and completed revisions. Prove whether each batch advances the backlog. Do not
  use the ten-ID log preview or `failed=0` at submission as completion evidence.
- [ ] Run `go test -race ./internal/daemon ./internal/conversation/semsearch -count=1`,
  isolated receiver restart/lost-acceptance scenarios, and `make check`. Commit as
  `Bound semantic preparation and recover accepted deliveries`.

### Task 13: Report effective runtime state without causing work

**Modify:** The config loader from Task 1,
[config watcher](../../../internal/daemon/config_watcher.go),
[config apply](../../../internal/daemon/config_apply.go), runtime and daemon
startup from Task 7, [control server](../../../internal/daemon/control_server.go),
[daemon status](../../../internal/daemon/status.go),
[operation registry](../../../internal/clispec/daemon_ops.go),
[CLI renderer](../../../internal/cli/daemon/daemon.go), and
[control protocol](../../../api/clyde/v1/daemon/service.proto).
**Create:** `internal/daemon/runtime_status.go`, `control_server_status.go`,
`client_runtime_status.go`, and their tests.

**Consumes:** Task 1 directions, Task 2 availability, current listener ownership,
and subsystem counter snapshots. **Produces:** One passive runtime status RPC
rendered by both CLI and MCP. New state ownership exists even when semantic
runtime construction is disabled.

```go
type ConfigurationSource string
type ConfigurationApplyState string
type ConfigurationStatus struct {
    Source ConfigurationSource
    AppliedGeneration string
    CandidateGeneration string
    CandidateState ConfigurationApplyState
    CandidateRoute config.Route
}
type ListenerState string
type ListenerStatus struct {
    Surface string
    Network string
    RequestedAddress string
    BoundAddress string
    Source ConfigurationSource
    State ListenerState
}
type WorkCounters struct {
    Component string
    Passes uint64
    ContentBytesRead uint64
    ContentRowsRead uint64
    BytesWritten uint64
    Pending uint64
    OldestPendingAt time.Time
}
type RuntimeStatus struct {
    WorkerPID int
    Configuration ConfigurationStatus
    Semantic semanticAvailabilitySnapshot
    Listeners []ListenerStatus
    Work []WorkCounters
}
```

Make enum values explicit for configuration sources and pending/rejected/applied
states. Listener states are disabled/listening/failed/unavailable. Map private
semantic state into typed protobuf messages, not JSON blobs.

- [ ] Refactor the existing loader to return `LoadedConfig{Config Config,
  Source ConfigSource, Generation string}` through
  `LoadGlobalOrDefaultWithSource() (LoadedConfig, error)`. `ConfigSource` is a
  typed defaults/file enum. Compute the generation digest from the same bytes
  used to decode configuration. Keep the existing loader as a wrapper. Do not
  reread the path later and accidentally attribute a different config revision.
- [ ] Publish immutable applied/candidate configuration status at validation,
  rejection, quiet-wait, successful apply, failed apply, and reload handoff.
  Preserve old applied state after rejection. Use captured daemon environment
  overrides, not the environment of a later status CLI invocation.
- [ ] Add a `GetRuntimeStatus` RPC with a meaningful request field such as
  `include_work_counters`; add a typed response matching the snapshot. Generate
  code with `make proto`, never edit generated files. Add
  `controlServer.runtimeStatus func() RuntimeStatus`, wired to
  `runtimeServices.snapshotStatus() RuntimeStatus`, and the client function
  `GetRuntimeStatus(ctx context.Context) (RuntimeStatus, error)`.
- [ ] Read existing runtime listener handles, extending their retained bind
  metadata where needed. Reuse MITM listener snapshot logic rather than creating
  another independently maintained port registry. Show actual assigned ports,
  Unix socket labels, and absent profiler state.
- [ ] Extend the existing operation registry to render the same status through
  CLI and MCP. Preserve the separate historical `--since` output contract and
  `TestDaemonMetricsStatusOutputHasOnlyStableHistoricalKeys`.
- [ ] Use a real Unix-socket control-server fixture. Call status repeatedly while
  semantic is disabled and unavailable; assert returned states and unchanged
  engine-attempt, scan, and projection counters. Stop the daemon and assert
  unavailable rather than disabled. Test explicit false versus default provenance.
- [ ] Run protocol/registry alignment tests, status renderer tests, daemon tests,
  and `make check`. Commit as `Expose passive effective daemon runtime status`.

### Task 14: Validate and track profiling listener handoff

**Modify:** [Debug facilities](../../../internal/daemon/debug.go), runtime from
Task 13, [listener inheritance](../../../internal/daemon/reload_inherit.go),
[supervisor listener metadata](../../../internal/daemonsupervisor/supervisor.go),
reload wiring from Task 7, and the existing profiling/reload tests.
**Create:** `internal/daemon/pprof_config.go`, `pprof_runtime.go`, and matching tests.

**Consumes:** Passive status, existing listener inheritance, and `livetrack`.
**Produces:** Explicit opt-in, loopback-only profiling that reports actual state
and survives unchanged hostname/port-zero reload.

```go
type PProfBindRequest struct {
    Address string
    Source ConfigurationSource
}
type inheritedListener struct {
    Listener net.Listener
    Spec daemonsupervisor.ListenerSpec
}
```

Add requested-address and source metadata to the existing `ListenerSpec`; keep
its actual address and descriptor validation. `resolvePProfBindRequest(configured,
environment string) PProfBindRequest` captures precedence in the daemon.

- [ ] Extend `pprof_reload_test.go` with real socket duplication for unchanged
  `localhost` and port zero. The current literal comparison should fail those
  cases. Preserve `reload_listener_test.go` descriptor restoration coverage.
- [ ] Implement `validatePProfBindRequest(request PProfBindRequest) error` and
  `validatePProfEndpoint(request PProfBindRequest, address net.Addr) error`.
  Accept only loopback literals or `localhost`, validate port syntax, and bind
  to explicit loopback addresses. Reuse/extract the existing `mitmBindAddrs`
  localhost resolution concern rather than copying another implementation.
- [ ] Compare requested identity with retained requested identity across handoff,
  then independently validate the inherited actual descriptor/address. Do not
  compare `localhost:0` directly with its allocated numeric endpoint. Reject
  fabricated or mismatched metadata. Keep changes confined to profiling unless
  a shared binder refactor passes all existing adapter tests unchanged.
- [ ] Make explicit listener-preserving reload reject additions, removals, and
  address changes. Preserve the watcher's topology-changing rebind route.
  Test both routes, including changes supplied by the environment override.
- [ ] Return a lifecycle-owned `pprofRuntime` from `startPProf` instead of
  launching an untracked server goroutine. Register before serving, retain
  listener identity for handoff, and expose `snapshot() ListenerStatus`.
  Unexpected serve exit becomes failed; intentional group drain does not.
- [ ] Serve a real profile, force listener failure, and inspect runtime status.
  Test disabled defaults, rejected wildcard/non-loopback addresses, actual
  allocated ports, active-request drain, and unchanged listener continuity.
- [ ] Run profiling, listener, rebind, lifecycle, and isolated reload tests plus
  `make check`. Commit as `Validate and track profiling listener inheritance`.

### Task 15: Verify the full work contract and prepare the breaking-change PR

**Modify:** Existing isolated live tests and the closest behavior documentation:
[conversation indexing](../../conversations.md),
[Cursor stores](../../cursor/stores.md),
[metrics](../../logging/metrics.md),
[reload behavior](../../reload-and-hot-apply.md), and
[testing guidance](../../testing/overview.md), only as each implementation lands.
**Create:** `test/live/incremental_work_live_test.go` and a standalone measurement
helper under `test/live` using the existing runtime dependencies.

**Consumes:** Every accepted task and the approved spec. **Produces:** Matched
baseline/candidate evidence, complete acceptance results, and a concrete PR.

- [ ] Freeze a representative corpus and replay the same workload against
  baseline and candidate. Include more workspaces than the connection budget,
  a large global store, an appending transcript, partial lines, repeated cached
  reads, semantic backlog, and a metrics interval boundary. Keep source fixtures
  outside the review diff and identify their content digests.
- [ ] Run separate engine-absent and engine-enabled conditions. Each quiet and
  active window lasts at least five minutes; repeat conditions. Record subsystem
  content bytes/rows, opens, hashes, cache writes, pending work, freshness latency,
  accepted/completed semantic revisions, metrics input bytes, CPU, and memory.
  Report median, 95th percentile, maximum, and totals where meaningful.
- [ ] Calibrate CPU units against an independent process CPU measurement. On
  Apple Silicon, use the measured Mach timebase rather than assuming nanoseconds.
  Keep physical disk counters separate from logical bytes read through cache.
  Retain sampler overhead and active-workload limitations in the evidence.
- [ ] Require zero content work for validated unchanged sources and zero semantic
  probes when disabled. Require progress for active and oversized inputs, bounded
  queues/connections, preserved conversation coverage, one writer through reload,
  and exact metrics after interruption. Report dirty global-store fallback costs
  separately. Do not declare the complete solution shipped while Task 11 remains
  blocked or a requested acceptance case has no proof.
- [ ] Run `make check`, `make test`, targeted race tests, and the isolated live
  suite. Do not run a second production `daemon run`, install, or reload the live
  service. Deployment proof requires separate authorization and a new live sample
  of the exact installed artifact.
- [ ] Update existing behavior documents in their current homes; do not copy the
  dated investigation into general documentation. Remove stale default-on and
  removed-key examples. Keep the approved spec as design history.
- [ ] Before a PR push, fetch, inspect every commit in `origin/main..HEAD`, and
  verify both signature status and raw `gpgsig` headers. Follow the repository PR
  workflow. The description must include the exact breaking-change notice:

  > Breaking change: Under `[conversation.semantic]`, replace `enabled` with
  > `ingestion_enabled`. `ingestion_enabled` and `search_enabled` both default to
  > `false` independently. Set `search_enabled = true` explicitly to search stored
  > conversations, even when ingestion is disabled. The removed `enabled` key now
  > causes a configuration error, including when false. Existing TOML is not
  > migrated automatically.

  Name what was actually reproduced, which protocol capabilities were verified,
  and whether live deployment was tested. Do not equate a spec commit with a fix.

## Coverage audit before execution

| Approved requirement | Planned task |
| --- | --- |
| Optional dependency, independent flags, hard cut, sandbox defaults | 1 and 3 |
| Shared retries, no hammering, disabled versus unavailable | 2 and 3 |
| URI correctness and repeated warnings | 4 and 5 |
| Shared early discovery, WAL validation, active-store limits | 5 |
| Append progress, resume links, partial records, cancellation | 6 |
| Freshness barriers, fair scheduling, watcher recovery | 7 |
| Atomic generations, cache migration, one writer across reload | 0B and 7 |
| Metrics byte cursors, pending requests, report equivalence | 8 and 9 |
| Metrics crash recovery, migration, retention | 8 and 10 |
| Receiver epochs, uncertain acceptance, oversized delivery | 11 and 12 |
| Effective configuration, passive CLI/MCP status, counters | 13 |
| Profiling opt-in, loopback safety, hostname/port-zero handoff | 14 |
| Paired sampling, complete checks, breaking-change announcement | 15 |

Execution must resolve two explicit feasibility gates rather than invent facts:
reliable unchanged SQLite validation under connection eviction in Task 5, and
receiver capabilities in Task 11. Neither gate permits dropping data, silently
repeating work, or marking incomplete behavior complete.
