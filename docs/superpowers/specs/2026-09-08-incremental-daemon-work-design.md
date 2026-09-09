# Process daemon work incrementally

Clyde should discover, persist, synchronize, and aggregate changed data without
repeatedly processing the entire retained corpus.

Status: proposed design for [issue 315](https://github.com/agoodkind/clyde/issues/315).
This specification defines intended behavior. It does not claim implementation
or deployment.

## Establish the measured problem

A four-minute observation on September 8, 2026, from 23:07:47 to 23:11:47 PDT
captured 121 process-counter readings and a concurrent two-minute stack sample.
The worker's version stamp and the inspected source reference were `1767db00`.
The installed binary also reported `vcs.modified=true`, so its stamp alone does
not establish exact source provenance.

| Observation | Result |
| --- | --- |
| Worker CPU | 46.94% of one core on average; 204.44% peak over two seconds |
| Resident memory | 1,102.5 to 1,233.3 MiB |
| OS-accounted disk bytes | 8.89 MiB read; 64.52 MiB written |
| Complete conversation cache writes | Four, approximately 6.1 MiB each |
| Cursor workspace failures | Six descriptors, three warnings each, four cycles |
| Semantic delivery | Four accepted submissions totaling 954,175 document records |
| Metrics aggregation | One 20,030 ms pass, with no new or pruned output records |

The CPU counters were calibrated against process CPU time. This Mac uses
24 million Mach ticks per second, not one billion counter units per second.
Disk counters exclude reads satisfied by the filesystem cache. These
measurements describe an active workstation and do not assign a cost percentage
to each subsystem. The conversation count changed during the observation.

Source inspection and sampled stacks establish several independent mechanisms:

- Cursor performs composer and legacy workspace discovery separately, before
  generic unchanged-record reuse. Composer revision discovery projects and
  hashes historical message rows.
- Every successful index refresh encodes and overwrites the complete cache.
  Ordinary cached reads can request another discovery pass.
- Metrics checkpoints identify a timestamp. They do not prevent reopening
  selected logs at byte zero and decoding their historical records.
- Semantic delivery materializes large sets of requested conversations.
  Submission success is distinct from completion of the asynchronous job.

The remote URI failure is independently reproducible:
`vscode-remote://ssh-remote%2Bexample/path` fails with `invalid URL escape "%2B"`.
The parser attempts host decoding before reaching its non-file URI branch.
Local file URIs, escaped spaces in local paths, and an unescaped remote authority
pass the same probe.

Semantic registration failures did not recur on the sampled workstation, where
the engine was running. That observation does not resolve the issue's report of
retries on a client without semantic search enabled. The four batches also do
not prove duplicate delivery: the evidence cannot distinguish advancing a backlog
from requesting previously submitted revisions again. Both cases require separate
acceptance proof.

### Account for every reported symptom

| Reported symptom | Investigation result | Required correction |
| --- | --- | --- |
| Repeated Cursor workspace reads | Source confirms repeated discovery before cache reuse; stacks confirm Cursor content reads. | Share discovery and validate revisions before content work. |
| Remote workspace warnings every minute | The encoded-authority probe reproduces the failure; one error logs at three layers. | Preserve remote identities and deduplicate unchanged failures. |
| Semantic registration retries without opt-in | An absent search setting enables the connection even when feeding is disabled; existing tests reproduce that default. | Require explicit opt-in and perform no dependency work when disabled. |
| Repeated warnings when the engine is absent | Retry delay already grows to a 30-second cap, but failed registration logs warnings at multiple layers. | Keep recovery for opted-in clients and report failure transitions once. |
| No discoverable profiling endpoint or status listener ports | Profiling is opt-in; status lacks a complete runtime listener inventory. | Keep profiling off by default and expose its effective state and actual address. |

The additional cache, metrics, and large semantic-delivery workloads measured
above remain in scope. A missing engine is distinct from an intentionally
disabled integration, and an intentionally disabled profiler is distinct from
a failed listener.

## Define scope and preserve contracts

The change covers conversation discovery, index persistence, Cursor workspace
identity, semantic opt-in and delivery, metrics aggregation, diagnostic visibility,
and their worker lifecycle.
It does not change adapter behavior, provider-owned storage, or public
conversation identifiers and export semantics.

The existing [Cursor reading contract](../../cursor/stores.md) remains binding,
including recovery of stored messages omitted from a composer's reference list.
An optimization must not discard those messages or treat a failed read as a
confirmed deletion.

The scheduler extends existing stamp, append-offset, parser, and lifecycle
primitives. It does not introduce a second conversation parser, a parallel
shutdown mechanism, or a replacement daemon process architecture.

## Give refresh one owner

One scheduler owns discovery work and publication of index snapshots. A snapshot
is an immutable set of records and source revisions identified by a monotonically
increasing generation number within its cache epoch. A cache epoch changes when
incompatible persisted state is rebuilt.

The scheduler accepts typed work requests containing a provider, source identity,
reason, and requested freshness. Repeated events merge by source. Work received
during a pass remains pending for a later pass; publication must not erase it.
When the pending set reaches its memory bound, it collapses to a dirty provider
root rather than dropping events.

| Trigger | Required behavior |
| --- | --- |
| Startup | Serve the last completed snapshot; reconcile sources in the background. |
| Cached listing or semantic snapshot read | Return the published generation without scheduling discovery. |
| Source event | Mark the affected source dirty and coalesce repeated notifications. |
| Explicit freshness request | Wait for relevant pending work or receive a typed deadline or incomplete-read error. |
| Unknown request or conversation identifier | Share one provider reconciliation; retain existing ambiguity and not-found semantics. |
| Lost event, overflow, or watcher failure | Mark coverage incomplete and schedule reconciliation. |

An explicit freshness request validates relevant source revisions after admission
and captures that validation as its barrier. It waits until the required work is
reflected in a published snapshot. An empty pending queue alone is not proof of
freshness. A lookup with unknown source coverage, including request identifiers
that may occur in several stores, requires provider-wide reconciliation.
Simultaneous lookups share that work. Later appends do not extend an admitted
barrier indefinitely. A deadline or incomplete validation returns an explicit
error rather than stale data labeled fresh.

Provider roots receive a lightweight metadata reconciliation at the existing
one-minute cadence. This pass discovers new directories and validates source
revisions; it must not parse unchanged content. Events make changed sources
eligible sooner. Normal coalescing cannot postpone eligibility beyond the
existing one-minute refresh interval. Explicit freshness bypasses coalescing.
Queue age and freshness lag remain observable when a backlog exceeds that bound.

Watchers are installed before startup reconciliation, and events arriving during
it are retained. Directory replacement, newly created subdirectories, watcher
resource exhaustion, and rename all enter the same reconciliation path.
Correctness must not depend on receiving every notification.

Scheduling is fair across providers. A large source cannot indefinitely starve
small changed sources. Work units have cancellation points and bounds on bytes,
rows, pending requests, and resident connections. Limits are named, centralized,
and verified against the acceptance workloads; they do not silently truncate
results. No new public configuration surface is required by this design.

## Cache discovery before reading content

Each provider maintains a discovery snapshot separately from parsed conversation
records. The snapshot records source identity, observed revision, completeness,
and the records contributed by that source.

Cursor shares one workspace inventory across composer metadata, legacy chats,
and request lookup. A changed workspace database is opened once for its discovery
snapshot, and the existing readers extract its supported records within that
snapshot. Global and workspace metadata keep their existing merge behavior.

Successful empty results are cached. Failed reads retain the last successful
contribution and mark it stale. A source is removed only after a successful
parent reconciliation proves absence. Access denial, database contention, and
partial reads must not remove conversations.

Revision validation includes database identity, database changes, write-ahead-log
changes, descriptor changes, and source replacement. The write-ahead log (WAL) is
the SQLite file that can contain committed changes before they reach the main
database. Main-database size and modification time alone are insufficient.

Read connections are bounded and read-only. A SQLite connection change counter
may be compared only on the same retained connection. Reopening or evicting a
connection invalidates that baseline. Reuse then requires a separately validated
database/WAL revision token; otherwise the source receives a new reconciliation
before it can be called unchanged. Persisted connection-local counters are never
such a token. Tests with more stores than the connection budget must prove that
eviction does not turn every idle minute into a content rescan. Failure of that
test blocks the discovery optimization rather than weakening revision checks.
Transactions are short enough to avoid holding old provider snapshots throughout
idle periods or downstream delivery. No provider checkpoint, trigger, migration,
or write is permitted.

A change during a scan leaves the source dirty for another pass. A partial scan
does not publish a supposedly complete replacement revision. Persisted discovery
state includes parser/schema version so changed interpretation invalidates old
positive and negative cache entries.

### Bound changes inside a store

An unchanged validated store requires no message projection or content hashing.
For a changed global Cursor store, one consistent reconciliation groups supported
rows by composer and computes the existing content-revision semantics. It
replaces independent per-composer database passes where the query plan permits.
Only changed composer contributions are reparsed and republished.

The provider currently offers no verified row change feed. The design therefore
does not promise append-only cost for arbitrary SQLite edits or deletions.
A dirty global store may still require a complete key/content reconciliation.
Its cost must be measured separately from unchanged-store and unrelated-workspace
costs. A narrower changed-row path can replace it only after real provider writes
prove detection of historical edits, deletions, metadata updates, and orphaned
stored messages. The complete reconciliation remains the recovery oracle.

Appendable transcript files reuse complete-line offsets and existing parser
state. Resume-link extraction consumes the same incremental source progress
rather than starting a second scan from byte zero. Truncation, replacement, or
an invalid prefix restarts that source. An incomplete final line remains pending.

## Preserve remote workspace identities

Scheme recognition precedes local filesystem URI parsing. Valid remote workspace
URIs retain their scheme, authority, and path without being converted into local
paths. Encoded remote authorities such as `ssh-remote%2Bexample` remain valid
remote identities. Local file URIs retain existing escaping and platform rules.

The boundary uses explicit local, remote, absent, unsupported, and invalid
outcomes. Remote identities are never passed to local filesystem operations.
Malformed descriptors do not hide readable conversations from their databases.

One boundary owns diagnostic logging. A repeat failure for an unchanged source
revision increments a counter without another warning. A changed descriptor is
retried; transient access failures use bounded retry scheduling even when its
content revision is unchanged. Recovery produces one event. Retryable failures
are never permanently negative-cached.

## Publish and persist changed generations

Publication compares records, metadata, deletion decisions, parser state, and
source revisions. An unchanged pass advances in-memory observation state without
rewriting the cache or emitting a semantic content change. Source-stamp changes
alone do not imply changed transcript content.

Changed generations retain the existing cache representation initially. The
writer stages a complete replacement beside the cache, flushes it, atomically
replaces the committed file, and completes the platform durability sequence.
Readers observe either the previous complete generation or the new complete
generation. They never read a partially overwritten file.

Only one daemon generation owns persistence. A reload child may serve a completed
snapshot while awaiting ownership, but it cannot scan and commit as a second
writer. Ownership transfer follows the existing daemon process-lock lifecycle;
it cannot depend on the child completing a refresh before reporting readiness.
The child rereads the final committed generation after acquiring ownership and
then reconciles events accumulated during handoff.

Watchers, connections, and refresh work register through `livetrack`. Group drain
stops admission, cancels active work, waits for it, and then releases storage.
Background scans must not strip lifecycle cancellation. Existing inherited
listeners and stream-drain behavior remain governed by the
[reload contract](../../reload-and-hot-apply.md).

A corrupt or incompatible cache is rebuilt from provider artifacts with an
explicit diagnostic. A failed write preserves the previous durable cache and
pending work. It does not report persistence success. Derived persistence may
move to per-record transactions later only if measurements show that changed
whole-cache generations remain a material cost.

## Require semantic search opt-in

### Establish the configuration and startup chain

At source revision `1767db00`, the configuration split introduced by commit
`03f7fadbb` on July 29, 2026, treats an omitted search setting as enabled.
The following chain explains the reported retry pattern without assuming an
explicit `search_enabled = false` was ignored.

| Layer | Verified behavior |
| --- | --- |
| Missing configuration | The [loader](https://github.com/agoodkind/clyde/blob/1767db00db7fd91946cf2ed7517b1cb294131764/internal/config/load.go#L219) builds defaults; parsing an omitted semantic section leaves its fields at their zero values. |
| Effective directions | The [direction resolver](https://github.com/agoodkind/clyde/blob/1767db00db7fd91946cf2ed7517b1cb294131764/internal/config/conversation_config.go#L65) returns false for feeding but true for an absent search setting; either direction enables engine use. |
| Connection startup | The [runtime](https://github.com/agoodkind/clyde/blob/1767db00db7fd91946cf2ed7517b1cb294131764/internal/daemon/conversation_semantic_runtime.go#L162) starts for either direction, attempts collection registration, and starts its retry worker on failure. |
| Feeder startup | The [daemon wiring](https://github.com/agoodkind/clyde/blob/1767db00db7fd91946cf2ed7517b1cb294131764/internal/daemon/run.go#L135) separately checks feeding, so connection retries do not prove that indexing is enabled. |
| Retry behavior | Registration uses a 10-second attempt deadline and exponential delay from one second to a 30-second cap. The connector and client can both warn for one failed registration. |
| Applying configuration | A semantic-setting change follows the existing reload route. Editing the file does not establish that the running generation has loaded it. |

The current direction tests passed during this investigation, including
`TestAnUnwrittenSearchSettingMeansOn`,
`TestStoppingTheWritesKeepsSearchReachable`, and all explicit direction pairs.
They demonstrate the defect's default policy, not the proposed fix. An omitted
section and `enabled = false` alone permit retries today. Both direction flags
explicitly false make the startup guard return without constructing a runtime.

The reporter's effective configuration and loaded generation were not captured.
If both flags were explicitly false in that generation, this default does not
explain those retries; investigation must then establish configuration source,
load success, and surviving worker ownership. The spec does not label that
different case reproduced.

### Define optional dependency behavior

Semantic search is optional. The proposed resolver keeps the two existing
directions but changes the omitted search setting to inherit `enabled`.
An explicit `search_enabled` value takes precedence. This preserves an explicit
read-only search configuration while removing implicit engine use.

| `enabled` | `search_enabled` | Feeding | Search | Engine runtime |
| --- | --- | --- | --- | --- |
| Omitted or false | Omitted | Off | Off | Not constructed |
| False | False | Off | Off | Not constructed |
| False or omitted | True | Off | On | Constructed |
| True | Omitted | On | On | Constructed |
| True | False | On | Off | Constructed |
| True | True | On | On | Constructed |

Installing the engine, finding its socket, retaining an old collection, or
configuring only an address or collection identifier never enables a direction.
The disabled path performs no socket resolution, filesystem dependency probe,
connection attempt, collection registration, retry scheduling, manifest request,
semantic projection, or journal initialization. Repeated status calls and rejected
semantic queries cannot start those operations indirectly.

Without the package installed, Clyde still starts and serves raw conversation
listing, retrieval, context, and export. A semantic query while disabled returns
a typed disabled result immediately. It does not report engine failure, attempt
installation, silently enable the feature, or return an empty successful search.
A query when explicitly enabled but unavailable returns a distinct unavailable
result. This change does not promise a new local substitute for semantic search.

The resolver is shared by startup, feeder admission, query admission, status, and
configuration transitions. Status reports the two effective directions, whether
each value was explicit or inherited, the loaded configuration generation, and
runtime state. It reports disabled from local state without checking installation.
It must not reinterpret configuration using the CLI process's environment.

An enabled-to-disabled reload stops new admission in the old generation, cancels
pending connection attempts and timers, joins the worker, and closes its client
through the existing lifecycle before the change is reported applied. Existing
remote jobs are not deleted or falsely reported cancelled; local delivery state
is retained for reconciliation after a later explicit enable. Inactive clients
have no retry worker. Re-enabling creates exactly one runtime.

The default change is intentional: clients that previously relied on
`enabled = false` with omitted `search_enabled` must explicitly set
`search_enabled = true` to retain search without feeding. Migration never rewrites
their configuration or infers consent from an existing collection. Examples and
generated default configurations must use the same off-by-default policy.

The current sandbox template explicitly enables both directions and uses the live
engine. Default sandbox validation must instead work without that dependency;
engine-backed validation requires an explicit sandbox opt-in. Tests for absent
settings must omit the section rather than use the existing harness shortcut
that writes both flags false. Otherwise the regression remains untested.
Explicit engine-maintenance commands remain deliberate one-shot operations;
their invocation must never create a persistent background retry loop.

### Recover only when explicitly enabled

For opted-in clients, an unavailable engine remains retryable with the existing
bounded attempt deadline and capped exponential backoff. The fix is not a longer
timeout and does not turn absence into permanent disablement. Startup of unrelated
daemon services must not wait for the first failed engine registration.

One runtime boundary owns the warning for entering an unavailable state. Client
helpers return typed errors without emitting the same warning again. Identical
failures update attempt counts, last error, and next attempt time in status rather
than producing another warning every cycle. A materially different failure or
recovery produces one transition event. Verbose per-attempt diagnostics remain
opt-in. Disabling the integration stops retries rather than merely hiding logs.

## Make enabled semantic delivery bounded and recoverable

The feeder consumes published generations without requesting discovery. It
retains the existing engine policy, projection rules, suppression behavior, and
busy-job gate. Discovery revisions and projected-content revisions are distinct:
metadata-only changes must not force unchanged transcript projection repeatedly.

Delivery state is keyed by collection identity, engine index epoch, projection
version, conversation identity, and content revision. An index epoch identifies
the receiver's current collection contents; rebuilding the receiver invalidates
old delivery acknowledgements. A local revision match cannot suppress a valid
receiver rebuild request.

| Delivery state | Required behavior |
| --- | --- |
| Needed | Queue a revision for bounded preparation. |
| Prepared | Persist a delivery identity before sending. |
| Accepted | Persist the returned job identity; do not call it indexed. |
| Unknown outcome | Reconcile acceptance or job status without immediately resending the whole batch. |
| Completed | Advance acknowledged revisions only after receiver success. |
| Failed or cancelled | Retain required revisions and retry under bounded backoff and existing policy. |

Batch limits apply to serialized bytes and documents before materializing the
entire needed set. Projection and transport consume bounded chunks. Splitting
one conversation must preserve the receiver's replacement semantics; partial
chunks cannot accidentally delete earlier chunks. Oversized conversations remain
visible as deferred work until bounded complete-conversation delivery is
supported. Permanent deferral is not an accepted implementation. Acceptance of
this stage requires demonstrated progress for conversations larger than the
batch limit. Backpressure limits outstanding accepted work as well as preparation.

A failed status lookup is an unknown outcome, not proof of job completion.
The opt-in and retry rules above also govern delivery reconnects.

Receiver idempotency and collection-epoch support must be verified before
implementing the durable-delivery stage. If the existing protocol cannot resolve
a lost acceptance response or transfer an oversized conversation safely, that
stage requires an explicit protocol extension before it can be accepted. Until
then, unknown outcomes and deferred conversations retain their work and remain
observable. They do not count as completed recovery. A client-side journal alone
cannot establish exactly-once delivery. Other stages do not depend on that
extension.

Verification correlates accepted jobs, terminal states, acknowledged revisions,
and needed revisions. The current ten-ID log preview cannot establish full batch
overlap. Bounded diagnostic counters and revision digests must show backlog
progress without logging transcript text or complete identity inventories.

## Aggregate only new metrics records

The background metrics worker resumes from source file identities and complete-line
byte offsets. It reuses the existing metrics event parser and aggregation rules.
Explicit historical reports retain their requested-window semantics; this change
removes repeated background reads, not the ability to query retained history.

The checkpoint includes source cursors, pending request aggregates, coverage
state, output identity, and committed output length. Non-metric records still
advance the cursor. Pending requests survive a pass and a restart, including
requests whose start and terminal events occur in different rotations.

Output append and checkpoint publication form one recoverable commit:

1. The sole writer holds the existing rollup write lock and validates its
   checkpoint against output identity and committed length.
2. It reads bounded complete input records and stages aggregate and cursor changes.
3. It appends output, flushes it, and durably replaces the checkpoint with the new
   committed length and pending state.
4. Readers use only the committed output boundary. After interruption, the writer
   discards its uncommitted output tail before replaying input from the checkpoint.

Generation markers use the same writer protocol. Recovery must never truncate
provider files or output with an identity that does not match the checkpoint.
Readers coordinate with the existing lock when snapshotting a checkpoint and
output boundary, so they cannot pair different commits.

Rotation completes the old file and discovers the new file by identity.
Truncation and replacement invalidate that source cursor. Compressed closed
rotations are processed once during migration or gap recovery, not on every pass.
Partial final lines remain uncommitted. Missing retained input or corrupt
checkpoint state reports incomplete coverage rather than inventing complete
history. Pending aggregation is bounded in memory and may spill to Clyde-owned
state; resource exhaustion must remain an explicit incomplete result.

Retention runs only when data can expire. It never rewrites retained output just
to discard an identical temporary copy. Retention writes and flushes a new output
generation without replacing the old one. It atomically publishes a checkpoint
selecting the new generation, identity, and committed boundary. The old generation
remains available until that checkpoint is durable and existing readers release
it. Recovery follows the committed checkpoint and cleans up only unreferenced
generations. This prevents a crash between output replacement and checkpoint
publication from destroying the last consistent pair. Cancellation is checked
inside bounded read batches, not only between files.

## Expose diagnostic state without enabling diagnostics

The profiler is intentionally off by default. The effective address comes from
`CLYDE_DEBUG_PPROF_ADDR` when nonempty, otherwise `debug.pprof_addr`. No resolved
address means no profiling listener. The issue's binary string about an inherited
listener being disabled is a conditional reload error; it does not establish
that this error occurred on the reporter's machine.

Current status output lacks a complete listener inventory. The revised status
uses the daemon's existing runtime listener records, extending them where needed,
and reports each surface as disabled, listening, failed, or unavailable. It
includes configured and actual bound addresses, the effective configuration
source, and daemon generation. A Unix control socket is reported as a socket,
not a missing TCP port. An unreachable daemon is unavailable, not presumed
disabled. CLI and MCP render the same typed status result.

Profiling remains an explicit opt-in through the existing setting. Status can
identify that setting when profiling is disabled, but viewing status never
enables an endpoint, refreshes conversations, or probes semantic search.
Routine subsystem counters remain available without profiling or an external
search engine. Configured port zero reports the actual assigned port.

Source inspection found no loopback validation before the profiler's generic TCP
bind, despite the config comment describing a loopback endpoint. The revised
path validates the final effective address, including environment overrides and
inherited listeners, before serving profiles. `localhost` resolves and binds only
to loopback. Wildcard and non-loopback addresses fail with a specific diagnostic.
This specification does not enable an externally reachable profiler.

Unchanged profiling settings preserve the inherited listener across reload.
The current binder compares the configured address directly with the actual
endpoint, which can differ for `localhost` or port zero. Handoff must preserve
the requested bind identity separately from the resolved endpoint and validate
both, rather than rejecting an unchanged setting because name resolution or
port allocation changed its representation.
Enable, disable, and address changes follow one explicit lifecycle contract:
the existing config-watcher rebind route may change topology, while explicit
listener-preserving reload rejects a topology change with an actionable error.
Status reports pending versus applied configuration rather than claiming a
listener changed before it did. Tests cover both routes. Profiling listener and
server ownership use the existing lifecycle registry, and unexpected server exit
changes status from listening to failed.

## Migrate without changing provider state

The conversation cache gains versioned discovery metadata. Existing caches remain
readable for startup and receive a background reconciliation before the new
revision baseline is trusted. Old derived state is not deleted before its
replacement is committed.

Metrics migration imports existing rollup output and the timestamp checkpoint
under the writer lock. It performs one reconciliation of available history,
deduplicates existing output using the established request/generation identity,
and commits byte cursors and pending state together. It does not start at the
current log end and silently skip earlier unaggregated work.

Semantic delivery journals start with unknown receiver acknowledgements and
reconcile them; migration cannot mark a backlog complete. Unsupported state
versions fail clearly. Source-format rollback must use the last compatible
committed derived state or a controlled rebuild, not concurrent old and new
writers.

## Deliver in independently verifiable stages

| Stage | Deliverable and gate |
| --- | --- |
| 1 | Correct semantic opt-in, prove dependency-free startup and disable transitions, and add effective status plus subsystem counters. |
| 2 | Correct remote identity handling, share workspace discovery, and skip unchanged stores before content reads. |
| 3 | Add scheduler ownership, pure cached reads, atomic changed-generation persistence, and reload handoff tests. |
| 4 | Add metrics byte cursors, recoverable output commits, migration, and retention scheduling. |
| 5 | Verify receiver protocol capabilities; add bounded semantic preparation and durable job reconciliation. |
| 6 | Measure active global-store reconciliation; add narrower row tracking only with proven provider semantics. |
| 7 | Complete profiling status, loopback validation, and activation/rebind coverage without enabling profiling by default. |

Stages 4 and 5 can follow independent implementation lanes after the shared
lifecycle contracts are established. Each stage preserves existing behavior
except the explicit opt-in and diagnostic changes defined here, and carries its
own regression and performance proof. No stage is accepted solely because
warnings disappear.

## Verify correctness and bounded work

Tests exercise public index, parser, daemon, and reporting boundaries with real
temporary files, writable fixture databases owned by the tests, and narrow fake
external engines. Assertions cover returned conversations, persisted generations,
delivered content, metrics totals, and measured I/O. Tests do not merely assert
that mocked helpers were called.

| Scenario | Required result |
| --- | --- |
| Missing config, omitted semantic section, or `enabled = false` alone; package and socket absent | Start successfully; perform zero semantic dependency probes, dials, registrations, retry wakes, or feeder passes over at least five minutes. |
| Same disabled cases with a sentinel engine socket present | Accept zero connections while raw operations, repeated status, and disabled semantic queries run. |
| Each explicit direction pair and inherited search value | Match the configuration table; search-only never feeds, and feed-only never answers semantic queries. |
| Enabled-to-disabled reload during dial, retry wait, or active connection | Apply disabled state, drain the old runtime, and perform zero later attempts or deliveries; re-enable creates one runtime. |
| Enabled integration with initially absent engine | Keep unrelated daemon services usable, retain capped retries with one unavailable warning, and recover when the fixture engine appears. |
| Default sandbox and examples | Do not silently enable semantic search; absent-setting tests exercise real omission. |
| Profiling unset, explicitly enabled, invalid, or failing | Report the correct effective state; bind only when opted in and only on validated loopback. |
| Profiling environment override, port zero, unchanged reload, or topology change | Report actual daemon-bound addresses and configuration source; exercise watcher rebind and explicit-reload rejection separately. |
| Profiling bound through `localhost` or port zero followed by unchanged reload | Preserve the existing socket and assigned port; compare retained bind identity rather than literal configured and resolved address strings. |
| Unchanged corpus with valid revision baselines and fully acknowledged semantic state | No content scans, message hashes, cache writes, or semantic projection; lightweight metadata validation is allowed. |
| Connection eviction, restart, or invalid revision baseline | Revalidate before reuse; missing proof triggers reconciliation, while repeated idle connection churn cannot cause repeated content scans. |
| One transcript append | Read only complete added content; preserve a partial tail and existing conversation identity. |
| One changed workspace store | Do not open unrelated workspace stores for content; update all affected contributions. |
| WAL-only commit, checkpoint, historical edit, or deletion | Detect the changed content without deleting unaffected conversations. |
| Empty, remote, legacy, orphaned-message, or conflicting metadata case | Preserve existing coverage, ordering, deduplication, and ambiguity behavior. |
| Access failure and recovery | Serve the last good contribution with explicit stale coverage; retry and publish recovery. |
| Rename, truncate, replace, missed event, or watcher overflow | Reconcile to the same result as a clean full scan. |
| Continuous event burst | Bound queue memory and make progress without starving another provider. |
| Reload or shutdown during scan and persistence | Keep one writer, complete cache files, no orphaned work, and existing listener continuity. |
| Semantic outage, lost acceptance, failed status lookup, receiver rebuild, restart, or oversized conversation | Preserve needed revisions and demonstrate bounded recovery using the verified receiver protocol; unresolved or permanently deferred work fails stage acceptance. |
| Metrics request spanning passes and rotations | Preserve exact totals and pending state across restart. |
| Interruption before append, after append, or during checkpoint publication | Commit each metrics result once or replay it without duplicate visible output. |
| Interruption before or after retention output creation, checkpoint selection, or old-generation cleanup | Recover the selected complete output/checkpoint pair and preserve committed totals. |
| Unchanged metrics input and no retention expiry | Read and write no history content and do not rewrite the checkpoint. |
| First migration and incompatible or corrupt state | Preserve recoverable history; report any coverage gap explicitly. |

Performance validation uses the same frozen corpus for baseline and candidate,
with quiet and active windows of at least five minutes each. The active workload
includes a growing transcript, a changing global Cursor database, repeated cached
queries, and a metrics interval crossing. Repeat each condition to separate
workload variation from the change.

Run the dependency-free and engine-enabled cases separately. Exercise missing
configuration and explicit values through the real loader and daemon entry point,
using temporary state roots and a counting Unix-socket fixture. Zero accepted
connections alone does not prove zero failed dials: also record dial/probe
attempts and retry-worker admission. Startup, disable, and recovery tests must
assert observable behavior and retain the raw-operation controls. The earlier
active-workstation sample is not acceptance for clients without the engine.

Acceptance requires zero redundant content work in the unchanged cases above.
For active cases, record stores opened, rows and bytes examined, content hashes,
cache bytes written, submitted and acknowledged semantic bytes/documents,
metrics bytes consumed, queue age, freshness latency, CPU, and resident memory.
Report median, 95th percentile, and maximum latency and memory, plus total CPU
and I/O.
Dirty global-store scans must appear separately so a corpus-sized fallback cannot
be hidden by averages.

No global percentage reduction is promised from the earlier active sample.
The performance gate is the work contract and a paired comparison with unchanged
coverage and bounded freshness. Idle memory must settle across repeated cycles;
bounded queues and connections must plateau under sustained load.

Implementation validation requires the normal repository checks, targeted race
tests for shared snapshots and handoff, and the existing isolated daemon test
harness. A live-service success claim additionally requires exact installed-build
identity and a new sampling window. This specification authorizes neither
installation nor deployment.
