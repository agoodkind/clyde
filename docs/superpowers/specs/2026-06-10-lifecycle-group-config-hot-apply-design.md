# Lifecycle group, quiet-wait reload, and in-process config apply

Date: 2026-06-10
Status: Draft for review

## Problem

A config file edit today always replaces the daemon process. The watcher
(`internal/daemon/config_watcher.go`) debounces, validates, and triggers
`ReloadDaemon()` (zero-bind-gap re-exec with FD inheritance) or
`RebindDaemon()` (listener topology changed). The reload drain caps at 60
seconds with a 5 second idle grace, so any exchange that outlives the cap is
severed, and every config-only edit pays full process churn.

Two features fix this, and both sit on top of one refactor:

1. **Quiet-wait**: the watcher defers a reload until no client exchange is in
   flight, bounded by a max wait.
2. **In-process config apply**: config-only edits rebuild the config-derived
   runtime pieces and swap them atomically, with no re-exec. In-flight
   exchanges finish on the config they started with.

The refactor is the headline change. The machinery both features need is
currently duplicated and hand-sequenced, and adding the features without the
refactor would create more copies of it.

## Current state (inventory, verified 2026-06-10)

Six livetrack registries, constructed independently:

| Registry | Site | Field |
| --- | --- | --- |
| Adapter ingress | `internal/adapter/livetrack_meta.go:64` | `Server.requests` |
| Adapter egress | `internal/adapter/livetrack_meta_egress.go:55` | `Server.egressRegistry` |
| Codex websocket sessions | `internal/adapter/codex/livetrack_meta_ws.go:36` | provider `wsSessionRegistry` |
| MITM tunnels (one per proxy) | `internal/mitm/proxy.go:125` | `Proxy.Tunnels` |
| Search jobs | `internal/conversation/search_jobs.go:139` | `SearchJobManager.registry` |
| Config watcher | `internal/daemon/config_watcher.go:90` | `configWatcher.registry` |

Two copies of the same poll-until-zero ticker loop:
`internal/livetrack/drain.go:208` (`waitForIdle`) and
`internal/adapter/server_routes.go:227` (`Server.WaitForIdle`). The second is
the hand-rolled equivalent `AGENTS.md` forbids.

Four hand-sequenced drain/shutdown orchestrations that encode order
imperatively and must stay consistent by hand:

- `startReloadDrain` (`internal/daemon/reload.go:297`): adapter
  `ShutdownWith` with idle grace, MITM proxies in a WaitGroup, then search
  jobs drain, search store close, semantic runtime close, capture store close.
- `runtimeServices.shutdown` (`internal/daemon/runtime.go:417`): watcher
  stop, listeners close, adapter shutdown, proxies, search jobs, stores.
- `adapter.Server.ShutdownWith` (`internal/adapter/server_routes.go:151`):
  keepalives off, ingress drain, `httpSrv.Shutdown`, egress drain, codex
  websocket drain.
- `mitm.Proxy.ShutdownWith` (`internal/mitm/proxy.go:218`): server shutdown,
  tunnel drain.

Five scattered timing constants: `reloadDrainCap` 60s, `reloadDrainIdleGrace`
5s, `daemonShutdownTimeout` 5s (`internal/daemon/run.go:24-26`), default
`PollEvery` 50ms and `CloserGrace` 2s (`internal/livetrack/registry.go:60-61`).

One standing `AGENTS.md` violation: capture-file flocks must register with
livetrack, and the capture store registers nothing; it is closed by a trailing
hand-written call in each sequence.

Two facts that shape the design:

- Adapter ingress sessions exist only while a handler runs, so idle keepalive
  connections never inflate that count (`server_routes.go:211-215`).
- MITM tunnel sessions are long-lived; parked Cloudflare keepalives sit
  registered indefinitely. Activity is tracked per write via `Session.Touch`,
  and `Session.IdleSince` distinguishes streaming from parked. The drain
  idle-grace fast path already uses this. `Snapshot` and `CountByPredicate`
  drop the session state pointer, so `IdleSince` reads zero through them;
  any active-count must walk live pointers under the registry lock.

## Headline refactor: `livetrack.Group`

One declarative lifecycle plan owns construction, waiting, draining, and
closing for every long-lived subsystem. The enforcement is the API shape, not
a test.

### Construction choke point

- `livetrack.New` becomes unexported. The only way to obtain a
  `Registry[M]` is the package-level generic function (package-level because
  Go methods cannot take type parameters):

  ```go
  func Attach[M Meta](g *Group, member MemberSpec, opts Options[M]) *Registry[M]
  ```

- `MemberSpec` declares, at construction time: `Phase`, `QuietRelevant`
  (counts toward "a client exchange is in flight"), and the member's
  `DrainOptions` (idle grace participation, parallel close).
- A registry outside the plan is a compile error, not a CI finding.

### Lifecycle choke point

- `Registry.Drain` and `Registry.DrainWith` become unexported. The public
  registry surface shrinks to register/release/touch/count/snapshot and
  `ForceCloseMatching` (kept for the parent-cascade adopters in
  `connect_tunnel.go`).
- `Group.Quiesce(ctx, reason, budget)` is the single lifecycle entry point.
  Phases run in declared order; members within a phase drain in parallel
  where declared safe; hooks run at their declared position.
- Hooks carry the non-registry steps the four sequences contain today:
  keepalives off, `httpSrv.Shutdown`, listener closes, store closes,
  semantic runtime close. Subsystems register hooks at construction; the
  daemon never hand-sequences again.

### Phases and budgets

Phases, in order: `PhaseIngress` (adapter requests, MITM tunnels, the HTTP
server hooks), `PhaseEgress` (adapter egress, codex websocket sessions),
`PhaseWorkers` (search jobs, semantic runtime, config watcher), `PhaseStorage`
(capture store, search store close hooks). The capture store becomes a
declared storage-phase member, fixing the standing flock-registration gap.

The config watcher member keeps its non-blocking cancel semantics: on the
reload path the watcher goroutine can be the caller blocked in its own reload
RPC, so its member spec declares cancel-then-proceed (the closer cancels the
loop context and `Quiesce` does not wait for its natural release on the
reload path), preserving what `cancelConfigWatcher` does today
(`internal/daemon/runtime.go:410`).

Budgets replace the scattered constants with two named profiles declared next
to the group: `BudgetReload{Cap: 60s, IdleGrace: 5s}` and
`BudgetShutdown{Cap: 5s}`. The three call sites become:

- reload drain: `group.Quiesce(ctx, "reload", BudgetReload)`
- daemon shutdown: `group.Quiesce(ctx, "shutdown", BudgetShutdown)`
- rebind drain: same `Quiesce` as reload, before the supervisor request, as
  today.

### One wait loop

The group owns a single ticker loop parameterized by a predicate, evaluated
across all members on the same tick:

- Drain wait: total count over draining members is zero.
- Quiet wait: `AwaitQuiet(ctx, grace)`; no `QuietRelevant` member has a
  session active within `grace`, where active means `IdleSince <= grace`
  walked over live pointers. Unlike `Quiesce` it mutates nothing: registries
  stay open and new sessions register freely while waiting.

Quiet-relevant members are the client-exchange surfaces: adapter ingress,
adapter egress, codex websocket sessions, and MITM tunnels. Search jobs, the
semantic runtime, and the config watcher are not quiet-relevant: they are
internal restartable work and must not hold a reload to max-wait.

Registry-level `waitForIdle` becomes this loop with a single member.
`adapter.Server.WaitForIdle` and `ActiveRequestCount` delegate to group/member
methods; their hand-rolled ticker is deleted.

### Deletions, not wrappers

`startReloadDrain`, the hand sequence inside `runtimeServices.shutdown`,
`adapter.Server.ShutdownWith`, `mitm.Proxy.ShutdownWith`, and
`adapter.Server.WaitForIdle` are removed in the same change, decomposed into
declared members and hooks. There is no parallel path left to drift back to;
reintroducing one requires re-adding a public API.

What the type system cannot see stays test-enforced: that a long-lived
goroutine registers a session at all remains under `livetrack_lints_test.go`,
extended so an unattached registry construction cannot appear (the compile
choke makes this structural; the lint guards the goroutine-registration half).

### Migration order (behavior bit-for-bit)

1. Pin current behavior with tests against today's code: drain ordering,
   budgets, force-close counts, idle-grace fast-path eviction.
2. Land `Group` alongside the existing API.
3. Move construction sites one subsystem at a time: adapter, codex provider,
   MITM, search jobs, stores, config watcher.
4. Delete the old entry points and the duplicated loops.
5. Land the two features on top.

## Feature 1: quiet-wait before reload and rebind

In `configWatcher.handleChange`, after parse validation and before
`trigger(...)`:

1. `group.AwaitQuiet(ctx, reloadDrainIdleGrace)` bounded by a max-wait
   constant declared beside the budget profiles (`reloadQuietWait`). No new
   config table; timing reuses the existing grace meaning ("idle this long
   means not streaming") and each registry's `PollEvery`.
2. Re-hash the config file. If the content reverted to the baseline hash
   during the wait, skip the reload entirely.
3. Trigger reload or rebind as routed.

Properties: a parked keepalive tunnel never blocks quiet; an active stream
does; steady traffic defers at most max-wait; a request longer than max-wait
still takes the existing drain. The wait applies to watcher-triggered reload
and rebind. CLI-triggered reloads (`clyde daemon reload`, `make deploy`) stay
prompt and do not wait; the helper is available if an opt-in flag is wanted
later.

## Feature 2: in-process config apply

### Routing decision

`validateReloadListenerConfig` generalizes into one typed decision consumed by
the watcher, the reload validation, and the apply path:

```go
func ClassifyConfigChange(runtime, oldCfg, newCfg) Route
// Route ∈ {RouteHotApply, RouteReload, RouteRebind, RouteRestartRequired}
```

The classifier is conservative: a changed field that is not explicitly
classified hot-appliable routes to reload, never silently no-op. The v1
reload-required set: adapter enable/host/port/cursor ingress port, MITM
listener set/hosts/ports, pprof address, capture store db path, MITM CA cert
and key paths, logging sink layout. Everything else under `[adapter]`,
`[mitm]` provider rules, `[mitm.drift]`, `[search]`,
`[conversation.semantic]`, and logging levels is hot-appliable in v1.

### Apply mechanics (clyde's generation handoff at object level)

- The daemon owns the current config in one holder
  (`atomic.Pointer[config.Config]`); it is the single source after startup.
- **Adapter**: everything the adapter builds from config moves into one
  immutable snapshot struct (model registry, resolver, client identity,
  reasoning levers, retry policies, notices, auth deps, wire baseline paths,
  passthrough tables, pricing). Each request loads the snapshot once at
  ingress and uses it throughout. Apply builds a new snapshot and swaps the
  pointer: in-flight exchanges finish on the snapshot they started with, new
  exchanges get the new one. Nothing is severed and nothing waits.
- **MITM proxies**: the config-derived routing and hook state swaps the same
  way. Established tunnels keep their behavior until they close, matching the
  `docs/cursor.md` drain contract (finish the in-flight request, then the
  client reconnects).
- **Workers** (drift loop, semantic runtime, search job manager): stop through
  their declared group phase membership, rebuild from the new config,
  re-attach. The group is the only stop/start path.
- After a successful apply the watcher updates its baseline hash and keeps
  looping (it does not exit, since no process replacement happened).
- On apply failure of any component, the apply aborts and the watcher falls
  back to the quiet-wait reload route, logging the component and error.

## Acceptance criteria

Isolation (must not touch the running daemon). All live validation runs in a
git worktree against a throwaway state root and a fake config. Set
`XDG_STATE_HOME`, `XDG_CONFIG_HOME`, and `XDG_RUNTIME_DIR` to temp dirs so the
test daemon owns its own socket, capture db, and logs. Every port in the fake
config differs from production: adapter 11434→21434, cursor ingress
11435→21435, each MITM listener 487xx→587xx, pprof off or `[::1]:0`. Never run
`make deploy`, never `launchctl bootout`/`bootstrap` the user's daemon, never
bind a production port. A pre-flight check fails the suite if the chosen ports
are already listening.

Unit and contract level:

- `make test lint fmt staticcheck staticcheck-extra deadcode audit build` all
  green from the worktree; `make fmt` leaves no diff.
- Behavior-pinned tests assert the pre-refactor drain order, budgets (cap 60s,
  idle-grace 5s, shutdown 5s), and force-close counts still hold after the
  move, bit-for-bit.
- A compile-level test proves the choke point: a registry constructed outside
  `Attach`/the group does not compile, and the public registry surface no
  longer exposes `New`/`Drain`/`DrainWith`.
- The existing `livetrack_lints_test.go` rule still fails on an unregistered
  long-lived goroutine, and now also fails if any constructed registry is not
  a group member; the capture store registers as a storage-phase member.

Live daemon validation (worktree binary, fake config). Boot the worktree
daemon on the fake ports. Verify:

- Quiet-wait: with an in-flight adapter request, edit a hot field; reload
  defers until the request finishes, then applies. Confirm via
  `daemon.config_watch.*` and livetrack logs in the temp state root, not UI.
- Quiet-wait bound: a request longer than max-wait still takes the drain;
  assert the cap fires.
- Hot apply: edit a config-only field (e.g. a model alias); assert no re-exec
  (pid unchanged) and the new value serves on the next request.
- Topology change: edit a fake port; assert it routes to quiet-wait +
  zero-bind-gap reload (pid changes, no bind gap).
- Idle keepalive does not block quiet; an active stream does.

Cloudflare long-run (optional, real Cursor). Point a Cursor instance's
`http.proxy` at the fake `app.cursor` MITM port (58725), open a long-lived IDE
backend connection, trigger a reload mid-connection, and confirm the in-flight
request completes on the old generation while the idle tunnel is force-closed
at idle-grace rather than pinning the cap. Read
`logs/providers/mitm/wire.jsonl` and the drain idle-force-closed event to
verify.

Teardown. Stop the worktree daemon, remove temp dirs, confirm the production
daemon and this session are untouched (its socket and pid unchanged
throughout).

## Out of scope (v1)

- Hot-applying listener topology, capture store db path, CA paths, or logging
  sink layout (these route to reload/rebind).
- Quiet-wait on CLI-triggered reloads (helper exists; not wired).
- An RPC surface for hot apply (watcher-triggered only in v1).

## Contracts preserved

- `AGENTS.md` Daemon Reload: re-exec with FD inheritance, rejection of
  listener moves, child lock rule, old-generation drain. The reload path is
  untouched except for when it is entered.
- `docs/cursor.md` MITM drain contract: finish the in-flight request, no
  fresh requests on a reused keepalive tunnel after drain starts.
- `AGENTS.md` Tracked Long-Lived Work: strengthened from rule to API shape.
