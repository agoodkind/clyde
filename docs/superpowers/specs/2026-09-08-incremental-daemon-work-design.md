# Reduce daemon background work

Clyde should spend less CPU and disk I/O on ordinary conversation use and fix the
bugs demonstrated by issue 315.

Status: scope revised September 9, 2026, at the user's request.
[Issue 315](https://github.com/agoodkind/clyde/issues/315) remains the scope anchor.
This is a specification, not an implementation or deployment claim.

## Bound the change to observed bugs and normal use

Performance takes priority over preserving Clyde-owned caches and derived state.
Do not add durability machinery. Recent derived data may be lost, rebuilt, or
processed again after termination. Provider-owned conversations remain read-only.

| Condition | Classification and evidence | Decision |
| --- | --- | --- |
| Semantic search is not installed or configured | Expected optional setup. The issue reports repeated missing-socket registration failures. | Fix implicit activation and reduce the existing retry frequency. |
| Ingestion disabled while search is enabled | Expected use explicitly requested by the user. | Keep the operations independent. |
| Cursor remote workspaces | Expected use. The encoded-authority parse failure was reproduced. | Fix parsing and repeated warnings. |
| Repeated scans of unchanged workspaces | Observed in source and sampled stacks. | Cache discovery before content reads. |
| New messages, workspace changes, archive changes, and SQLite WAL updates | Normal application writes. | Detect them with existing metadata checks and ordinary refreshes. |
| Full conversation-cache writes every refresh | Four writes observed in four minutes. | Skip writes when cached content and progress have not changed. |
| Metrics repeatedly decoding old logs | One sampled pass took 20 seconds and produced no new records. | Resume from byte offsets and avoid unnecessary retention rewrites. |
| Large semantic preparation batches | Four submissions contained 954,175 document records. Duplicate work was not established. | Bound preparation; investigate actual overlap before proposing deduplication changes. |
| Missing profiling/port visibility in status | Reported in the issue; status lacks a complete listener view. | Show existing effective settings and bound addresses. |
| Ordinary configuration changes and daemon reloads | Existing supported user operations. | Keep current lifecycle behavior and stop replaced workers normally. |
| Updating across this approved data-format break | The operator will run a one-time reset after updating. | Discard old Clyde databases and derived index state; preserve configuration and reinstall Clyde's service. |
| Power loss, system crash, torn writes, corrupt checkpoints, or lost delivery acknowledgements | No incident evidence in this workstream. | Exclude recovery guarantees and fault-injection work. |
| Extended engine failure and transport failure combinations | Only the missing-engine retry loop was reported. | Do not build a general outage-recovery framework. |

Four-minute sampling recorded 46.94% average CPU relative to one core, a 204.44%
two-second peak, 1,102.5 to 1,233.3 MiB resident memory, and OS-accounted reads and
writes of 8.89 MiB and 64.52 MiB. CPU counter units were calibrated. This was an
active workstation, so the sample does not assign a cost share to each subsystem.

Source inspection used revision `1767db00`. The installed binary had that stamp
but also reported modified build inputs. The measurements establish runtime
activity; the stamp alone does not prove exact source provenance.

## Make ingestion and search independent opt-ins

Under `conversation.semantic`:

| Field | Operation | Default |
| --- | --- | --- |
| `ingestion_enabled` | Send conversations to the semantic engine for indexing. | `false` |
| `search_enabled` | Search conversations already indexed by the engine. | `false` |

Neither field enables the other. Ingestion disabled does not disable search or
delete indexed conversations. Both operations need the engine, but raw
conversation discovery, listing, reading, context, and export do not.

| Ingestion | Search | Engine use |
| --- | --- | --- |
| Off | Off | No runtime, socket checks, connection attempts, or retries. |
| On | Off | Ingest only; semantic queries report disabled. |
| Off | On | Search only; no ingestion preparation or submission. |
| On | On | Share the existing engine runtime. |

The current bug is a default policy: `enabled = false` stops feeding while an
omitted `search_enabled` still resolves to true. Existing direction tests
confirmed this behavior. The missing-engine report is not resolved merely because
the engine happened to be available on the sampled workstation.

Replace `conversation.semantic.enabled` with `ingestion_enabled` as a hard cut.
Reject the removed key, including false or mixed old/new configurations. Keep
`search_enabled` as the canonical spelling with its new false default. There are
no aliases, automatic configuration edits, or compatibility defaults.

Use Go fields `IngestionEnabled` and `SearchEnabled`, and JSON names
`ingestionEnabled` and `searchEnabled`. Update existing configuration producers,
examples, and tests together. Do not add another configuration input format.
Unrelated `enabled` settings keep their current meaning.

The implementation PR description must include:

> Breaking change: Under `[conversation.semantic]`, replace `enabled` with
> `ingestion_enabled`. Both `ingestion_enabled` and `search_enabled` default
> independently to `false`. Set `search_enabled = true` to search existing
> indexed conversations without ingestion. The removed `enabled` key causes a
> configuration error, even when false. Existing TOML is not migrated automatically.
> After updating and making the required configuration-key edits, run
> `clyde daemon hard-reset` once. This deletes Clyde's local databases and derived
> index state while preserving `config.toml`. Old Clyde-local database formats
> are unsupported; LMS is not reset.

Default sandbox configuration must also leave both operations off. Engine-backed
sandbox testing requires explicit opt-in. Tests for omission must actually omit
the fields rather than writing explicit false values.

## Reset Clyde after the breaking update

Provide the operator command `clyde daemon hard-reset` in the new version.
It destroys Clyde-owned database contents and rebuildable index/metrics state.
The operator runs it once after updating; old stored data is not migrated or
expected to work. This document does not claim the command is implemented yet.

The command performs this sequence:

1. Read configuration and resolve Clyde's service registration and exact data
   targets without opening any old database. Validate the new configuration
   before teardown; a removed configuration key must be corrected manually.
2. Stop and unregister Clyde's service so the service manager cannot respawn it.
   Stop its supervisor and worker processes, including a draining old worker,
   and wait until they no longer hold the data files.
3. Delete every Clyde-owned database in the reset inventory, its SQLite sidecars,
   and the derived index/metrics files that could retain incompatible state.
4. Re-run the existing native Go daemon service installer using the updated
   executable. Recreate Clyde's registration and start the daemon from the
   preserved configuration. Do not download, rebuild, or replace the binary.
5. Report the deleted targets and the fresh daemon's resulting status.

Reuse the Go deploy path used by `clyde daemon deploy`, not a shell installer or
`clyde install hooks`. The hook installer writes other applications' settings
and is outside this command's scope. Factor service removal into the same native
deploy package so install and reset share service paths and platform handling.
The reinstalled service must resolve the same preserved config and Clyde data
roots. Persist supported root overrides in its native service environment;
preserving the file is insufficient if the new daemon reads a different path.

On macOS, remove only Clyde's launchd job and its own LaunchAgent registration.
On Linux, stop/disable only Clyde's user service, remove its unit registration,
and refresh the user service manager. The relevant Clyde service registration
files remain in scope even though the OS stores them outside Clyde's data folder.
Never use a broad process-name match or remove another application's service.

The typed reset inventory includes the capture database at the resolved Clyde
capture-store path, its `-wal`, `-shm`, and `-journal` files, the conversation index
and saved append offsets, and the metrics rollup/checkpoint with their own
temporary or lock companions. The current source has one writable SQLite store;
new Clyde-owned stores and known retired database paths must join this same
inventory. Deletion uses paths, not an attempt to decode an old database schema.

Use the existing Clyde path resolvers, including supported XDG and storage-path
overrides. Remove exact owned files, not entire state, cache, config, runtime,
or repository directories. If a target resolves outside Clyde's owned scope,
reject that target before teardown rather than widening deletion scope. Never
follow a deletion path into another application's data. These ownership checks
enforce the command's requested boundary; they do not add recovery machinery.

Preserve `config.toml` byte for byte, along with credentials, CA certificates and
keys, raw logs, exported transcripts, provider settings, and provider databases.
Do not touch repository checkouts or sibling tools such as `desktop-via-clyde`.
Remove only Clyde's stale runtime socket/lock files after its processes stop.
Do not wipe shared parent directories or the installed executable.

Do not stop, unregister, reinstall, delete, reset, or migrate LMS, its files,
collections, or registration. The reset path makes no LMS administration calls.
The user explicitly permits the restarted Clyde daemon to resume ordinary
ingestion/search traffic enabled by the preserved configuration. That normal
traffic does not authorize an LMS reset or collection deletion.

Missing reset targets are already cleared. A teardown, deletion, or install
failure returns the failed step and does not claim completion. There are no
backups, compatibility readers, data migrations, or rollback procedures. After
deletion, an install failure may leave Clyde stopped; rerunning the command can
finish installation.

## Stop repeated missing-engine attempts

Reuse the existing shared runtime and retry worker. Do not create an
availability framework, socket watcher, persisted cooldown, or second retry loop.

When both flags are false, construct no semantic runtime. Status and semantic
queries cannot activate it indirectly. A disabled query returns a clear disabled
result; an opted-in query with no current client returns unavailable promptly.

When an opted-in client cannot register because the engine is absent, use
in-memory exponential backoff with base delays of 30 seconds, one minute, two
minutes, four minutes, and five minutes. Jitter stays within 30 seconds and five
minutes. Keep the existing separate 10-second attempt deadline. Repeated queries
must not initiate attempts or reset the timer.

Record one warning for entering the unavailable state, then update counters
without warning on every identical failure. Record recovery once. Remove duplicate
warnings from lower helpers for the same returned error.

Keep the existing startup path and the guard that stops ingestion preparation
while no client is available. Disabling both flags stops the retry worker through
the existing lifecycle.
A new process may restart the retry schedule; preserving it across termination
is not required.

No new handling is planned for uncertain acceptance, transport failure sequences,
or exact recovery after an engine rebuild. Existing behavior outside the reported
missing-engine case stays unchanged unless normal-use measurements expose a bug.

## Avoid unchanged Cursor content reads

Keep the existing periodic refresh cadence and single-flight refresh owner.
Do not add a filesystem-watcher framework, new scheduler, queue arbitration,
source epochs, or a new public freshness API.

Share workspace discovery across composer metadata, legacy chats, and request
lookup. Cache each source's successful or empty result before expensive database
reads. Use database, WAL, and descriptor metadata to decide what changed. A WAL
is SQLite's write-ahead log; changes can be committed there while the main
database file remains unchanged.

During a refresh, open a changed workspace store once for the existing readers.
Reuse unchanged source results. Treat ordinary database contention or an unreadable
descriptor as a failed read, not evidence that a conversation was deleted.
Recheck it on a later ordinary pass. Remove a contribution when normal discovery
confirms its source is gone.

Do not introduce retained connection pools or persistent `data_version` baselines.
Keep the existing read-only database boundary and short snapshots. Use existing
file stamps, including WAL and descriptor stamps, rather than designing a new
cross-platform file-identity or integrity subsystem.

For a changed global store, group work into one pass where practical and preserve
the existing stored-message and metadata semantics. A changed store may still need
a full pass. Measure that cost; do not invent a row-change feed or protocol project
without evidence it is needed.

Recognize remote URI schemes before local filesystem URI parsing. Preserve
`vscode-remote://ssh-remote%2Bexample/path` as a remote identity. Local file paths
and escaped spaces retain their current behavior. Keep readable conversations
even if their workspace descriptor is invalid.

Report an unchanged descriptor failure once at the owning boundary. Cache its
result by descriptor stamp so every refresh does not parse and log it again.
A changed descriptor or later successful read is processed normally.

## Keep refresh and cache writes simple

Cached list and status reads do not trigger extra discovery. Explicit freshness
requests use the existing refresh method and share an in-flight pass. They keep
current not-found and ambiguity behavior.

Reuse existing append offsets where the parser supports them. Process complete
new records and leave a partial final line for the next pass. Avoid the separate
resume-link reread of an unchanged prefix when the same decoded records can
supply those links. Normal truncation or replacement restarts that source.

Do not add cryptographic prefix verification, serialized parser journals, or
tests for arbitrary historical mutation hidden behind unchanged file metadata.

Write the conversation cache only when records or saved progress change.
Use ordinary writes; pre-upgrade formats need no compatibility support. Do not add file sync,
directory sync, backup generations, transaction manifests, or durability helpers.
A fresh in-memory result must not require a durability guarantee.

Use the current lifecycle to cancel and join refresh work during ordinary reload
or shutdown. Do not add a generation handoff protocol. After the required upgrade
reset, initialize current-format state. Later starts use that cache when readable
and rebuild it otherwise; do not add readers for pre-upgrade formats.

## Read only new metrics log bytes

Keep the existing event parser, aggregate calculations, output format, and
retention period. Replace timestamp-only background replay with a file position
and complete-line offset.

Keep pending request aggregates in memory between passes. Retain simple source
offsets in the existing checkpoint through ordinary writes, batched with useful
work. Unchanged input does not rewrite the checkpoint. Log rotation and requests
spanning ordinary passes remain covered.

Do not coordinate output append and checkpoint replacement as a transaction.
Do not add committed-length readers, output generations, recovery journals,
pending-state persistence, shared reader locks, or crash-consistency tests.

Use a valid saved offset when starting. If offset state is missing or incompatible,
start from the current active log end and report that historical summary coverage
is unavailable. Do not replay retained history solely to reconstruct derived state.
Previously written summaries remain readable. Unfinished aggregates may be lost
and some work may repeat after a restart; that tradeoff is accepted.
Ordinary status reports missing summary coverage rather than silently rebuilding
it through a full history scan.

Only run retention work when data can expire. Avoid writing a temporary copy
when nothing expires. Keep the ordinary existing retention mechanism.
Explicit user requests for historical reports keep their current behavior.

## Bound semantic preparation

Do not materialize all requested conversations before sending any work.
Prepare complete conversations into bounded batches using the existing projection
and streaming client. Process a conversation larger than the usual batch target
alone through the existing supported path; do not require a new receiver protocol.

Preserve selected content, message indexes, and existing receiver replacement
semantics. Keep the existing in-memory active-job guard and content memo.
Investigate repeated work by comparing actual jobs and conversation revisions.
Change deduplication only if that comparison demonstrates redundant work.

Do not add durable delivery identities, collection epochs, accepted-job journals,
lost-acknowledgement reconciliation, or exactly-once guarantees. A restart may
repeat a batch. The scope is normal workload memory/CPU and directly reproduced
redundant work.

## Show existing diagnostic state

Report effective ingestion/search flags, current semantic availability, next
retry time, and existing bound listener addresses from the daemon's own state.
Reading status does not probe semantic search or refresh conversations.
Keep disabled distinct from unavailable.

Profiling remains opt-in and uses the existing supported local configuration.
Show disabled versus an actual bound endpoint, including its assigned port.
Do not enable profiling to collect routine counters.

Do not add a profiling lifecycle redesign, configuration provenance journal,
new configuration generation protocol, or inherited-descriptor hardening.
Changes to profiling behavior require a reproduced normal-use bug; the current
reported problem is visibility.

## Validate only the scoped behavior

| Scenario | Required proof |
| --- | --- |
| Missing semantic configuration and no engine installed | Raw operations work; zero semantic probes or retries. |
| Ingestion off, search on, engine available | Stored search results return without ingestion work. |
| Explicit opt-in with missing engine | The existing worker follows the reduced retry schedule; identical failures do not flood logs. |
| Queries during the retry delay | Return promptly and do not cause another attempt. |
| Remote Cursor workspace | The reproduced URI loads and its conversations remain visible. |
| Repeated refresh without source changes | No unchanged database content reads or full-cache writes. |
| Normal messages, metadata changes, and WAL writes | Updated conversations appear on the next normal refresh. |
| Appended transcript and partial final line | New complete records appear without a second prefix scan. |
| Ordinary reload/configuration change | Replaced workers stop and the new settings take effect. |
| Operator hard reset after updating | Clyde stops and unregisters before deletion; all inventoried databases/sidecars and derived state are removed, configuration is unchanged, and the native installer starts fresh Clyde state. |
| Hard-reset ownership | LMS, provider stores, sibling tools, credentials, certificates, logs, exports, and repositories remain untouched by reset; normal configured traffic may resume after install. |
| New metrics records and routine rotation | Read new bytes and preserve calculations across ordinary passes. |
| No new metrics data and no expired output | No history reread, checkpoint rewrite, or retention rewrite. |
| Large ordinary semantic backlog | Lower peak preparation memory without losing expected conversation content. |
| Status and profiling visibility | Report existing runtime state without creating work. |

Repeat matched quiet and active samples of at least five minutes. Compare CPU,
resident memory, source bytes/rows read, cache bytes written, retries, and
semantic batch sizes. Keep raw-operation and returned-content controls.

Power-loss, system-crash, arbitrary corruption, interrupted-write, lost-acknowledgement,
and rare transport-combination tests are excluded. Do not expand the scope because
such cases are imaginable. Reset tests use isolated fixtures; no live reset,
installation, or deployment is authorized by this planning task.
