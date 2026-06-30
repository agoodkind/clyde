# Daemon reload, quiet-wait, and in-process config apply

The daemon owns one declarative lifecycle plan, `internal/livetrack.Group`, that
governs every long-lived registry and every non-registry teardown step. A config
edit no longer always re-execs the process: the watcher classifies the change and
either applies it in process, waits for quiet then reloads, rebinds, or asks for
a restart.

## The lifecycle group

Every subsystem that owns long-lived state registers it with the daemon's group
through `livetrack.Attach`, which is the only way to construct a registry.
`livetrack.New`, `Registry.Drain`, and `Registry.DrainWith` are unexported, so a
registry that is not a group member cannot be built and a registry cannot be
drained outside the group. `internal/livetrack/group_choke_test.go` enforces both
at the API surface.

Members declare a `MemberSpec` at attach time:

- `Phase` places the member in the drain order: `PhaseIngress` (adapter
  requests, MITM tunnels), `PhaseEgress` (adapter egress, codex websocket
  sessions), `PhaseWorkers` (search jobs, semantic connection, config watcher),
  `PhaseStorage` (capture and search stores).
- `QuietRelevant` marks the client-exchange surfaces whose active sessions hold
  the quiet-wait.
- `CancelNoWait` marks the config watcher, whose loop is cancelled rather than
  waited for on the reload path.

`Group.Quiesce(ctx, reason, budget)` is the single lifecycle entry point. It runs
each phase in order: before-hooks, then the phase's member drains concurrently,
then after-hooks. Before/after hooks reproduce the per-subsystem ordering the old
hand-sequenced drain used (adapter keepalives-off before ingress drain, HTTP
server shutdown after ingress but before egress, store closes after every surface
drains). Reload uses `budgetReload` (60s cap, 5s idle-grace); shutdown uses
`budgetShutdown` (5s cap). Both live in `internal/daemon/lifecycle_group.go`.

`Group.AwaitQuiet(ctx, grace)` is the one wait loop. It blocks until no
quiet-relevant member has a session active within `grace`, evaluated across all
members on a single tick, and mutates nothing so registrations continue while it
waits.

## Quiet-wait before reload

When the watcher decides to reload or rebind, it first calls `AwaitQuiet` over
the lifecycle group, bounded by `reloadQuietWait` (30s). A reload defers until
in-flight client exchanges finish, so it does not sever them. A parked keepalive
tunnel does not block quiet (its idle exceeds the grace); a streaming exchange
does. A request longer than the bound still takes the drain. After the wait the
watcher re-hashes the file, so an edit reverted during the wait applies nothing.

## In-process config apply

`config.ClassifyConfigChange(old, new)` returns one route:

- `RouteHotApply`: every changed field is in the hot set, so the daemon swaps the
  affected config-derived state with no re-exec. The hot set is the adapter
  model-registry inputs (`Models`, `Families`, `Pricing`, `DefaultModel`,
  `PassthroughOverrides`, `OpenAICompatPassthrough`) and the MITM proxy provider
  routing (`Providers`).
- `RouteReload`: a field changed that the running generation cannot swap but does
  not move a listener (capture-store db path, MITM CA paths), or any field not in
  the hot set. The conservative default for an unclassified change is reload.
- `RouteRebind`: a listener address moved (adapter host/port, cursor ingress
  port, MITM listener host/port, pprof address), so the new worker binds fresh.
- `RouteRestartRequired`: a whole surface was toggled (adapter or MITM
  enable/disable), which the running generation cannot honor; the operator must
  restart.

On `RouteHotApply` the daemon calls `applyConfigInProcess`, which swaps the
adapter model registry behind an atomic pointer and each MITM proxy's
config-derived routing under its lock, then advances the current-config pointer.
In-flight requests keep the registry and routing they loaded; new requests get
the swapped state. On any apply failure the daemon does not advance the pointer
and the watcher falls back to the quiet-wait reload path.

## What is preserved

- Zero-bind-gap reload: re-exec inherits the open listener file descriptors. The
  reload path is unchanged except for when it is entered. See
  `internal/daemon/reload.go`.
- MITM drain contract: an established tunnel finishes its in-flight request, then
  the client reconnects to the new generation rather than serving fresh requests
  on a reused keepalive tunnel. See `docs/cursor.md`.

## Validation

`internal/livetrack`, `internal/config`, `internal/adapter`, `internal/mitm`, and
`internal/daemon` carry unit tests for the group, classifier, and apply paths.
`test/live/` holds build-tagged (`-tags live`) daemon scenarios that boot the
binary in isolated temp XDG roots on fake ports, with a preflight that refuses to
run if a fake port is taken and a guard that asserts the production daemon is
untouched. Run them with `go test -tags live ./test/live/`.
