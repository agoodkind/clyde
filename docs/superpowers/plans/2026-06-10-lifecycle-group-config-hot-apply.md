# Lifecycle Group, Quiet-Wait Reload, and Config Hot-Apply Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace clyde's six independently-constructed livetrack registries and four hand-sequenced drain paths with one declarative `livetrack.Group`, then land two consumers on it: a quiet-wait that defers reload until no client exchange is in flight, and an in-process config apply that swaps config-derived state without re-exec.

**Architecture:** A `livetrack.Group` owns construction, waiting, draining, and closing for every long-lived subsystem. `livetrack.New` becomes unexported, so the only way to obtain a `Registry[M]` is `Attach[M](group, MemberSpec, Options[M])`, and an unattached registry is a compile error. `Registry.Drain` and `DrainWith` become unexported, so `Group.Quiesce(ctx, reason, budget)` is the single lifecycle entry point that runs declared phases in order with declared hooks. `Group.AwaitQuiet(ctx, grace)` is the one wait loop. The watcher gains `ClassifyConfigChange` returning hot-apply, reload, rebind, or restart-required; hot-apply swaps an immutable adapter config snapshot and MITM routing state and restarts workers through their group phases.

**Tech Stack:** Go 1.x, `internal/livetrack` (generics), `internal/daemon` (supervisor reload, fsnotify watcher), `internal/adapter`, `internal/mitm`, `internal/conversation`, fsnotify, gRPC, SQLite stores.

---

## Validation Contract (binds every live step)

All live validation runs in a git worktree against a throwaway state root and a fake config. The harness from Task 0.2 enforces this. Hard rules:

- Set `XDG_STATE_HOME`, `XDG_CONFIG_HOME`, `XDG_RUNTIME_DIR` to temp dirs so the test daemon owns its own socket, capture db, and logs.
- Fake config ports differ from production: adapter 11434 becomes 21434, cursor ingress 11435 becomes 21435, each MITM listener 487xx becomes 587xx, pprof off or `[::1]:0`.
- Never run `make deploy`, never `launchctl bootout`/`bootstrap` the user's daemon, never bind a production port.
- A pre-flight check fails the suite if any chosen port is already listening.
- Teardown stops the worktree daemon, removes temp dirs, and confirms the production daemon's socket and pid are unchanged.

Spec: `docs/superpowers/specs/2026-06-10-lifecycle-group-config-hot-apply-design.md`.

---

## File Structure

**New files:**
- `internal/livetrack/group.go`: `Group`, `MemberSpec`, `Phase`, `Budget`, `Attach`, `Quiesce`, `AwaitQuiet`.
- `internal/livetrack/group_member.go`: type-erased member interface (`drainMember`) and the adapter that erases `Registry[M]`.
- `internal/livetrack/group_test.go`: group unit tests.
- `internal/livetrack/group_choke_test.go`: compile-surface assertions (no exported `New`, `Drain`, `DrainWith`).
- `internal/config/change_class.go`: `Route` enum and `ClassifyConfigChange`.
- `internal/config/change_class_test.go`: classifier table tests.
- `internal/adapter/runtime_snapshot.go`: immutable `ConfigSnapshot`, `atomic.Pointer` holder, `ApplyConfig`.
- `internal/adapter/runtime_snapshot_test.go`: snapshot swap tests.
- `internal/daemon/lifecycle_group.go`: daemon-owned group assembly (`newLifecycleGroup`), budget profiles, `awaitDaemonQuiet`.
- `internal/daemon/config_apply.go`: `applyConfigInProcess` and worker restart helpers.
- `internal/daemon/config_apply_test.go`: apply routing and worker restart tests.
- `internal/daemon/reload_behavior_test.go`: characterization tests pinning pre-refactor drain order, budgets, counts.
- `test/live/reload_live_test.go`: live daemon validation scenarios (build-tagged `live`).
- `test/live/harness.go`: worktree, temp-root, fake-config, port-preflight helpers (build-tagged `live`).
- `docs/reload-and-hot-apply.md`: operator and agent doc for the new lifecycle.

**Modified files:**
- `internal/livetrack/registry.go`: unexport `New` to `newRegistry`; keep public register/release/touch/count/snapshot/forceclose; add `ActiveCount(grace)`.
- `internal/livetrack/drain.go`: unexport `Drain`/`DrainWith` to `drain`/`drainWith`; `waitForIdle` reused by group.
- `internal/livetrack/livetrack_lints_test.go`: add the "every constructed registry is a group member" assertion.
- `internal/adapter/server.go`, `server_routes.go`, `livetrack_meta.go`, `livetrack_meta_egress.go`: registries via `Attach`; delete the `WaitForIdle` ticker; `ShutdownWith` decomposed into hooks and members; load `ConfigSnapshot` at ingress.
- `internal/adapter/codex/livetrack_meta_ws.go` and provider wiring: ws registry via `Attach`.
- `internal/mitm/proxy.go`: `Tunnels` via `Attach`; `ShutdownWith` decomposed.
- `internal/conversation/search_jobs.go`: registry via `Attach`; restartable worker member.
- `internal/daemon/config_watcher.go`: registry via `Attach`; quiet-wait plus classify routing; baseline update on apply.
- `internal/daemon/reload.go`: `startReloadDrain`/`closeStoresOnReload` deleted, replaced by `group.Quiesce`.
- `internal/daemon/runtime.go`: `shutdown` hand-sequence replaced by `group.Quiesce`; group held on `runtimeServices`.
- `internal/daemon/run.go`: timing constants move to budget profiles in `lifecycle_group.go`.
- `internal/config/config.go` and loaders: no schema change for v1 (classifier reads existing fields).

---

## Phase 0: Worktree and validation harness

### Task 0.1: Create the isolated worktree

**Files:** none (workspace setup)

- [ ] **Step 1: Create the worktree via the using-git-worktrees skill**

Per the goal's isolation mandate, all work happens off the running checkout. From `/Users/agoodkind/Sites/clyde-dev/clyde`:

Run: `git worktree add ../clyde-lifecycle -b config-auto-reload-lifecycle`
Expected: `Preparing worktree (new branch 'config-auto-reload-lifecycle')` and a new dir `../clyde-lifecycle`.

If the repo uses the superpowers worktree helper instead, invoke `superpowers:using-git-worktrees` and let it place the worktree. All subsequent paths in this plan are relative to the worktree root.

- [ ] **Step 2: Confirm the worktree builds before any change**

Run: `cd ../clyde-lifecycle && make build`
Expected: build succeeds, producing the `clyde` binary under the worktree's `dist/` (or configured output).

- [ ] **Step 3: Proceed to 0.2** (no commit yet).

### Task 0.2: Live validation harness (worktree, temp roots, fake config, port preflight)

**Files:**
- Create: `test/live/harness.go`
- Create: `test/live/reload_live_test.go`

- [ ] **Step 1: Write the failing harness preflight test**

`test/live/reload_live_test.go`:

```go
//go:build live

package live

import "testing"

// TestPreflightRejectsProductionPorts asserts the harness refuses to run
// when a chosen fake port is already bound, so a live run can never collide
// with a real daemon.
func TestPreflightRejectsProductionPorts(t *testing.T) {
	h := newHarness(t)
	// Bind one fake port ourselves, then assert preflight fails on it.
	stop := h.occupyPort(t, h.cfg.AdapterPort)
	defer stop()
	if err := h.preflight(); err == nil {
		t.Fatal("preflight must fail when a fake port is already listening")
	}
}
```

- [ ] **Step 2: Run it to verify it fails to compile (harness absent)**

Run: `go test -tags live ./test/live/ -run TestPreflight -v`
Expected: FAIL with `undefined: newHarness`.

- [ ] **Step 3: Implement the harness**

`test/live/harness.go`:

```go
//go:build live

package live

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// fakePorts holds the throwaway ports a live daemon binds. Every value
// differs from the production default so a live run can never touch the
// user's daemon.
type fakePorts struct {
	AdapterPort       int // 21434 (prod 11434)
	CursorIngressPort int // 21435 (prod 11435)
	MITMBase          int // 58700.. (prod 487xx)
}

// harness owns the temp state/config/runtime roots and the fake config for
// one live daemon. All daemon I/O is redirected into temp dirs via the XDG
// env vars so nothing lands in the user's real clyde state.
type harness struct {
	stateRoot   string
	configRoot  string
	runtimeRoot string
	cfg         fakePorts
	binPath     string
	prodPidPre  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		stateRoot:   t.TempDir(),
		configRoot:  t.TempDir(),
		runtimeRoot: t.TempDir(),
		cfg: fakePorts{
			AdapterPort:       21434,
			CursorIngressPort: 21435,
			MITMBase:          58700,
		},
		binPath:    buildWorktreeBinary(t),
		prodPidPre: readPidFile(productionPidPath()),
	}
	return h
}

// preflight fails when any fake port is already listening, so the suite
// aborts before a collision instead of binding over a live service.
func (h *harness) preflight() error {
	for _, port := range []int{h.cfg.AdapterPort, h.cfg.CursorIngressPort, h.cfg.MITMBase} {
		if portListening(port) {
			return fmt.Errorf("preflight: port %d already listening; refusing to run", port)
		}
	}
	return nil
}

// occupyPort binds a fake port so a test can assert preflight rejects it.
func (h *harness) occupyPort(t *testing.T, port int) func() {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		t.Fatalf("occupy port %d: %v", port, err)
	}
	return func() { _ = ln.Close() }
}

func portListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("[::1]:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// env returns the XDG overrides that point the daemon at temp roots. Every
// live daemon command runs with this env so its socket, capture db, and logs
// stay inside t.TempDir().
func (h *harness) env() []string {
	return append(os.Environ(),
		"XDG_STATE_HOME="+h.stateRoot,
		"XDG_CONFIG_HOME="+h.configRoot,
		"XDG_RUNTIME_DIR="+h.runtimeRoot,
	)
}

func buildWorktreeBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "clyde-live")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/clyde")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build worktree binary: %v", err)
	}
	return out
}

func productionPidPath() string { return os.ExpandEnv("$HOME/.local/state/clyde/daemon.pid") }

func readPidFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var pid int
	_, _ = fmt.Sscanf(string(data), "%d", &pid)
	return pid
}

// assertProductionUntouched confirms the real daemon's pid file is unchanged
// across the live run, proving isolation held.
func (h *harness) assertProductionUntouched(t *testing.T) {
	t.Helper()
	if got := readPidFile(productionPidPath()); got != h.prodPidPre {
		t.Fatalf("production daemon pid changed during live run: %d -> %d", h.prodPidPre, got)
	}
}
```

Adjust `productionPidPath` to the real path from `internal/config` if it differs; the worktree's config package is the source of truth.

- [ ] **Step 4: Run the preflight test to verify it passes**

Run: `go test -tags live ./test/live/ -run TestPreflightRejectsProductionPorts -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add test/live/harness.go test/live/reload_live_test.go
git commit -m "Add live daemon validation harness with port preflight"
```

---

## Phase 1: Pin current behavior (characterization)

### Task 1.1: Characterize the reload drain order, budgets, and force-close counts

**Files:**
- Create: `internal/daemon/reload_behavior_test.go`

These tests lock in today's behavior so the refactor can prove bit-for-bit equivalence. They assert against the existing `startReloadDrain` path before it is deleted.

- [ ] **Step 1: Write the failing characterization test for drain budgets**

`internal/daemon/reload_behavior_test.go`:

```go
package daemon

import (
	"testing"
	"time"
)

// TestReloadDrainBudgetsAreStable pins the three drain timing values the
// refactor must preserve. If the lifecycle-group migration changes any of
// them, this fails and forces an intentional decision.
func TestReloadDrainBudgetsAreStable(t *testing.T) {
	if reloadDrainCap != 60*time.Second {
		t.Errorf("reloadDrainCap = %v, want 60s", reloadDrainCap)
	}
	if reloadDrainIdleGrace != 5*time.Second {
		t.Errorf("reloadDrainIdleGrace = %v, want 5s", reloadDrainIdleGrace)
	}
	if daemonShutdownTimeout != 5*time.Second {
		t.Errorf("daemonShutdownTimeout = %v, want 5s", daemonShutdownTimeout)
	}
}
```

- [ ] **Step 2: Run it to verify it passes against current code**

Run: `go test ./internal/daemon/ -run TestReloadDrainBudgetsAreStable -v`
Expected: PASS (these constants exist today at `run.go:33-35`).

- [ ] **Step 3: Add a drain-order characterization test using a recording runtime**

Append to `reload_behavior_test.go` a test that builds a `runtimeServices` with stub adapter/MITM/store members that append their name to an ordered slice when drained, runs `startReloadDrain`, and asserts the observed order: adapter and MITM proxies drain concurrently (both before stores), then search jobs, then search store, then semantic, then capture store. Use the existing fields on `runtimeServices`; inject stubs via the same construction the package tests already use.

```go
// TestReloadDrainClosesStoresAfterSurfaces asserts stores close only after
// the public surfaces (adapter, MITM) have drained, so SQLite WALs flush
// before a child reopens them. This ordering is the contract the
// lifecycle-group PhaseStorage must reproduce.
func TestReloadDrainClosesStoresAfterSurfaces(t *testing.T) {
	order := newOrderRecorder()
	r := newRecordingRuntime(t, order) // stubs append to order on drain/close
	done := r.startReloadDrain(testContext(t), testLogger(t))
	<-done
	order.assertBefore(t, "adapter", "capture_store")
	order.assertBefore(t, "mitm", "capture_store")
	order.assertBefore(t, "search_jobs", "search_store")
}
```

Implement `newOrderRecorder`, `newRecordingRuntime`, `testContext`, `testLogger` in the test file using the package's existing test helpers where present; otherwise define minimal local ones. If `runtimeServices` cannot be built with stubs without production listeners, narrow this test to the `closeStoresOnReload` helper, which takes only the store fields.

- [ ] **Step 4: Run the order test against current code**

Run: `go test ./internal/daemon/ -run TestReloadDrainClosesStoresAfterSurfaces -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/reload_behavior_test.go
git commit -m "Pin reload drain budgets and store-close ordering with characterization tests"
```

### Task 1.2: Characterize the idle-grace fast-path eviction

**Files:**
- Modify: `internal/livetrack/drain_test.go`

- [ ] **Step 1: Confirm an existing test already covers idle-grace eviction**

Run: `go test ./internal/livetrack/ -run Idle -v`
Expected: existing idle-grace tests pass. If none asserts "a session idle longer than grace is force-closed at drain start while a freshly-touched session survives to the deadline path", add one now:

```go
// TestDrainIdleGraceEvictsWedgedKeepsActive pins the fast-path the group's
// AwaitQuiet and Quiesce both depend on: idle-past-grace sessions are
// force-closed immediately; recently-touched sessions are not.
func TestDrainIdleGraceEvictsWedgedKeepsActive(t *testing.T) {
	// build registry with injected clock; register two sessions; Touch one;
	// advance clock past grace; DrainWith(IdleGrace: grace); assert the
	// untouched one force-closed and the touched one rode the deadline path.
}
```

- [ ] **Step 2: Run to verify it passes**

Run: `go test ./internal/livetrack/ -run TestDrainIdleGraceEvictsWedgedKeepsActive -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/livetrack/drain_test.go
git commit -m "Pin livetrack idle-grace fast-path eviction behavior"
```

---

## Phase 2: Build `livetrack.Group` alongside the existing API

The group lands first as additive surface. Existing registries keep working through `New`/`Drain` until Phase 3 migrates them and Phase 4 unexports the old entry points.

### Task 2.1: Phase, Budget, and MemberSpec types

**Files:**
- Create: `internal/livetrack/group.go`

- [ ] **Step 1: Write the failing test for phase ordering**

`internal/livetrack/group_test.go`:

```go
package livetrack

import "testing"

// TestPhaseOrder pins the declared lifecycle order: ingress drains before
// egress, egress before workers, workers before storage.
func TestPhaseOrder(t *testing.T) {
	want := []Phase{PhaseIngress, PhaseEgress, PhaseWorkers, PhaseStorage}
	got := orderedPhases()
	if len(got) != len(want) {
		t.Fatalf("orderedPhases len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("phase[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/livetrack/ -run TestPhaseOrder -v`
Expected: FAIL with `undefined: orderedPhases`.

- [ ] **Step 3: Implement phase/budget/memberspec types**

`internal/livetrack/group.go`:

```go
package livetrack

import "time"

// Phase names a lifecycle stage. Quiesce runs phases in orderedPhases order;
// members within a phase drain together. The order encodes the dependency
// chain: stop taking client traffic (ingress), finish provider calls
// (egress), stop internal workers, then close storage so SQLite WALs flush
// last.
type Phase uint8

const (
	PhaseIngress Phase = iota
	PhaseEgress
	PhaseWorkers
	PhaseStorage
)

func orderedPhases() []Phase {
	return []Phase{PhaseIngress, PhaseEgress, PhaseWorkers, PhaseStorage}
}

// String returns a stable label for slog records.
func (p Phase) String() string {
	switch p {
	case PhaseIngress:
		return "ingress"
	case PhaseEgress:
		return "egress"
	case PhaseWorkers:
		return "workers"
	case PhaseStorage:
		return "storage"
	default:
		return "unknown"
	}
}

// Budget bounds one Quiesce call. Cap is the overall deadline; IdleGrace is
// passed to each member's drain so wedged keepalive sessions are force-closed
// at drain start rather than waited out against Cap.
type Budget struct {
	Cap       time.Duration
	IdleGrace time.Duration
}

// MemberSpec declares how a registry participates in the group at Attach time.
// Phase places the member in the drain order. QuietRelevant marks a member
// whose active sessions count toward AwaitQuiet (the client-exchange
// surfaces). CancelNoWait marks a member whose closer is invoked but whose
// natural release is not waited for on the reload path (the config watcher,
// which can be the caller blocked in its own reload RPC).
type MemberSpec struct {
	Phase         Phase
	QuietRelevant bool
	CancelNoWait  bool
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/livetrack/ -run TestPhaseOrder -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/livetrack/group.go internal/livetrack/group_test.go
git commit -m "Add livetrack Phase, Budget, and MemberSpec types"
```

### Task 2.2: Type-erased member interface and registry adapter

**Files:**
- Create: `internal/livetrack/group_member.go`
- Modify: `internal/livetrack/registry.go` (add `ActiveCount`)

- [ ] **Step 1: Write the failing test for ActiveCount over live pointers**

Append to `group_test.go`:

```go
// TestActiveCountUsesLiveActivity asserts ActiveCount(grace) walks live
// session pointers so IdleSince reflects real activity, unlike Snapshot which
// drops the state pointer and would read zero.
func TestActiveCountUsesLiveActivity(t *testing.T) {
	clock := newFakeClock()
	r := New[testMeta](Options[testMeta]{Now: clock.now})
	s, _ := r.Register(context.Background(), "k", testMeta{}, noopCloser{})
	clock.advance(10 * time.Second)
	if got := r.ActiveCount(5 * time.Second); got != 0 {
		t.Fatalf("idle session counted active: got %d, want 0", got)
	}
	s.Touch()
	if got := r.ActiveCount(5 * time.Second); got != 1 {
		t.Fatalf("touched session not counted active: got %d, want 1", got)
	}
}
```

This uses `New`/`Register` because Phase 2 is additive; Task 4.1 renames these to `newRegistry`/`register` and updates this call. Add `testMeta`, `noopCloser`, and `newFakeClock` helpers in the test file if the package lacks them.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/livetrack/ -run TestActiveCountUsesLiveActivity -v`
Expected: FAIL with `r.ActiveCount undefined`.

- [ ] **Step 3: Implement ActiveCount on Registry**

Add to `internal/livetrack/registry.go`:

```go
// ActiveCount returns the number of tracked sessions whose last activity is
// within grace, walking live session pointers under the registry lock so
// IdleSince reads real activity. Snapshot and CountByPredicate drop the state
// pointer and would read IdleSince as zero, so the group's quiet check cannot
// use them. A grace <= 0 counts every tracked session as active.
func (r *Registry[M]) ActiveCount(grace time.Duration) int {
	now := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, sess := range r.sessions {
		if grace <= 0 || sess.IdleSince(now) <= grace {
			count++
		}
	}
	return count
}
```

- [ ] **Step 4: Implement the type-erased member interface**

`internal/livetrack/group_member.go`:

```go
package livetrack

import (
	"context"
	"time"
)

// drainMember is the type-erased view of a registry the Group drives. Each
// Registry[M] erases to this via registryMember so the group can hold members
// of different Meta types in one slice.
type drainMember interface {
	component() string
	count() int
	activeCount(grace time.Duration) int
	drainWith(ctx context.Context, reason string, opts DrainOptions) DrainResult
	spec() MemberSpec
}

// registryMember erases a Registry[M] to drainMember. It is constructed by
// Attach and stored on the Group; callers never build it directly.
type registryMember[M Meta] struct {
	reg     *Registry[M]
	memSpec MemberSpec
}

func (m registryMember[M]) component() string { return m.reg.component }
func (m registryMember[M]) count() int        { return m.reg.Count() }
func (m registryMember[M]) activeCount(grace time.Duration) int {
	return m.reg.ActiveCount(grace)
}
func (m registryMember[M]) drainWith(ctx context.Context, reason string, opts DrainOptions) DrainResult {
	return m.reg.DrainWith(ctx, reason, opts)
}
func (m registryMember[M]) spec() MemberSpec { return m.memSpec }
```

`drainWith` calls `r.reg.DrainWith` for now; Task 4.1 renames the method to `drainWith` and flips this call.

- [ ] **Step 5: Run tests to verify ActiveCount passes**

Run: `go test ./internal/livetrack/ -run TestActiveCountUsesLiveActivity -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/livetrack/registry.go internal/livetrack/group_member.go internal/livetrack/group_test.go
git commit -m "Add ActiveCount and type-erased drainMember for livetrack group"
```

### Task 2.3: `Group`, `Attach`, hooks, and `Quiesce`

**Files:**
- Modify: `internal/livetrack/group.go`

- [ ] **Step 1: Write the failing Quiesce ordering test**

Append to `group_test.go`:

```go
// TestQuiesceRunsPhasesInOrder asserts Quiesce runs hooks in phase order, and
// that a hook at PhaseStorage runs after every ingress hook has run.
func TestQuiesceRunsPhasesInOrder(t *testing.T) {
	g := NewGroup(GroupOptions{Log: testLogger(t)})
	var mu sync.Mutex
	order := []string{}
	record := func(name string) func(context.Context) error {
		return func(context.Context) error {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}
	g.AddHook(PhaseIngress, "ingress.hook", record("ingress"))
	g.AddHook(PhaseStorage, "storage.hook", record("storage"))
	g.Quiesce(context.Background(), "test", Budget{Cap: time.Second})
	if len(order) != 2 || order[0] != "ingress" || order[1] != "storage" {
		t.Fatalf("hook order = %v, want [ingress storage]", order)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/livetrack/ -run TestQuiesceRunsPhasesInOrder -v`
Expected: FAIL with `undefined: NewGroup`.

- [ ] **Step 3: Implement Group, NewGroup, Attach, AddHook, Quiesce**

Append to `internal/livetrack/group.go` (add imports `context`, `log/slog`, `sync`):

```go
// GroupOptions configures a Group.
type GroupOptions struct {
	Log *slog.Logger
}

// hook is a named non-registry lifecycle step (keepalives-off,
// http.Server.Shutdown, store close) run at a declared phase.
type hook struct {
	name string
	fn   func(context.Context) error
}

// Group is the single declarative lifecycle plan for a daemon generation. It
// owns every long-lived registry (added via Attach) and every non-registry
// step (added via AddHook), and exposes the only public lifecycle entry
// points: Quiesce (ordered drain) and AwaitQuiet (the one wait loop).
type Group struct {
	log     *slog.Logger
	mu      sync.Mutex
	members []drainMember
	hooks   map[Phase][]hook
}

// NewGroup constructs an empty group.
func NewGroup(opts GroupOptions) *Group {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Group{log: log, mu: sync.Mutex{}, members: nil, hooks: map[Phase][]hook{}}
}

// MemberCount reports how many registries are attached. Used by tests to
// assert a subsystem joined the group.
func (g *Group) MemberCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.members)
}

// Attach constructs a Registry[M] bound to this group. It is the only way to
// obtain a registry once New is unexported, so a registry that is not a group
// member cannot be constructed. Package-level (not a method) because Go methods
// cannot take type parameters.
func Attach[M Meta](g *Group, member MemberSpec, opts Options[M]) *Registry[M] {
	reg := New[M](opts)
	g.mu.Lock()
	g.members = append(g.members, registryMember[M]{reg: reg, memSpec: member})
	g.mu.Unlock()
	return reg
}

// AddHook registers a named non-registry step to run at phase during Quiesce.
func (g *Group) AddHook(phase Phase, name string, fn func(context.Context) error) {
	g.mu.Lock()
	g.hooks[phase] = append(g.hooks[phase], hook{name: name, fn: fn})
	g.mu.Unlock()
}

// Quiesce runs the declared lifecycle to completion under budget. For each
// phase in order it drains every member in that phase (concurrently) and runs
// every hook for that phase, bounded by budget.Cap overall and
// budget.IdleGrace per member drain.
func (g *Group) Quiesce(parent context.Context, reason string, budget Budget) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), budget.Cap)
	defer cancel()
	for _, phase := range orderedPhases() {
		g.quiescePhase(ctx, phase, reason, budget)
	}
}

func (g *Group) quiescePhase(ctx context.Context, phase Phase, reason string, budget Budget) {
	g.mu.Lock()
	members := make([]drainMember, 0)
	for _, m := range g.members {
		if m.spec().Phase == phase {
			members = append(members, m)
		}
	}
	hooks := append([]hook(nil), g.hooks[phase]...)
	g.mu.Unlock()

	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(m drainMember) {
			defer wg.Done()
			defer recoverHook(ctx, g.log, "livetrack.group.member_panic", m.component())
			m.drainWith(ctx, reason, DrainOptions{IdleGrace: budget.IdleGrace})
		}(m)
	}
	wg.Wait()
	for _, h := range hooks {
		func(h hook) {
			defer recoverHook(ctx, g.log, "livetrack.group.hook_panic", h.name)
			if err := h.fn(ctx); err != nil {
				g.log.WarnContext(ctx, "livetrack.group.hook_failed", "concern", "livetrack",
					"phase", phase.String(), "hook", h.name, "err", err)
			}
		}(h)
	}
}

func recoverHook(ctx context.Context, log *slog.Logger, event, name string) {
	if r := recover(); r != nil {
		log.ErrorContext(ctx, event, "concern", "livetrack", "name", name, "err", r)
	}
}
```

`Attach` calls `New[M]` and `registryMember.drainWith` calls `DrainWith` for now; Task 4.1 flips both to the unexported names. Member drains run concurrently within a phase to match the WaitGroup fan-out in today's `startReloadDrain`.

- [ ] **Step 4: Run the Quiesce test to verify it passes**

Run: `go test ./internal/livetrack/ -run TestQuiesceRunsPhasesInOrder -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/livetrack/group.go internal/livetrack/group_test.go
git commit -m "Add livetrack Group with Attach, hooks, and ordered Quiesce"
```

### Task 2.4: `AwaitQuiet` (the one wait loop)

**Files:**
- Modify: `internal/livetrack/group.go`

- [ ] **Step 1: Write the failing AwaitQuiet test**

Append to `group_test.go`:

```go
// TestAwaitQuietReturnsWhenActive asserts AwaitQuiet returns once no
// QuietRelevant member has a session active within grace, and that a
// non-quiet-relevant member (a worker) does not hold it open.
func TestAwaitQuietReturnsWhenActive(t *testing.T) {
	g := NewGroup(GroupOptions{Log: testLogger(t)})
	clock := newFakeClock()
	ingress := Attach[testMeta](g, MemberSpec{Phase: PhaseIngress, QuietRelevant: true},
		Options[testMeta]{Now: clock.now, PollEvery: time.Millisecond})
	worker := Attach[testMeta](g, MemberSpec{Phase: PhaseWorkers, QuietRelevant: false},
		Options[testMeta]{Now: clock.now, PollEvery: time.Millisecond})
	_, _ = worker.Register(context.Background(), "job", testMeta{}, noopCloser{}) // never blocks quiet
	s, _ := ingress.Register(context.Background(), "req", testMeta{}, noopCloser{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		ingress.Release(context.Background(), s, "done")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if quiet := g.AwaitQuiet(ctx, 5*time.Second); !quiet {
		t.Fatal("AwaitQuiet returned not-quiet despite released ingress session")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/livetrack/ -run TestAwaitQuietReturnsWhenActive -v`
Expected: FAIL with `g.AwaitQuiet undefined`.

- [ ] **Step 3: Implement AwaitQuiet**

Append to `group.go` (add `time` import if not present):

```go
// quietPollEvery is the AwaitQuiet poll cadence, matching the registry
// default poll so reload timing stays symmetric across surfaces.
const quietPollEvery = 50 * time.Millisecond

// AwaitQuiet blocks until no QuietRelevant member has a session active within
// grace, or ctx expires. It mutates nothing: registries stay open and new
// sessions register freely while waiting, so this is safe to call before a
// reload that may not happen. Returns true if quiet was reached, false if ctx
// fired first. The single ticker evaluates the quiet predicate across all
// quiet-relevant members on one tick, so quiet is one coherent condition
// rather than N sequential waits.
func (g *Group) AwaitQuiet(ctx context.Context, grace time.Duration) bool {
	if g.quietActiveCount(grace) == 0 {
		return true
	}
	ticker := time.NewTicker(quietPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return g.quietActiveCount(grace) == 0
		case <-ticker.C:
			if g.quietActiveCount(grace) == 0 {
				return true
			}
		}
	}
}

func (g *Group) quietActiveCount(grace time.Duration) int {
	g.mu.Lock()
	members := append([]drainMember(nil), g.members...)
	g.mu.Unlock()
	total := 0
	for _, m := range members {
		if m.spec().QuietRelevant {
			total += m.activeCount(grace)
		}
	}
	return total
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/livetrack/ -run TestAwaitQuietReturnsWhenActive -v`
Expected: PASS.

- [ ] **Step 5: Run the full livetrack package**

Run: `go test ./internal/livetrack/ -v`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/livetrack/group.go internal/livetrack/group_test.go
git commit -m "Add livetrack Group.AwaitQuiet single wait loop"
```

---

## Phase 3: Migrate construction sites onto the group

Each subsystem moves to `Attach`, one commit each, with the daemon building the group and passing it in. Behavior stays identical because the group still uses `New`/`DrainWith` under the hood until Phase 4.

### Task 3.1: Daemon builds the group and holds it

**Files:**
- Create: `internal/daemon/lifecycle_group.go`
- Create: `internal/daemon/lifecycle_group_test.go`
- Modify: `internal/daemon/runtime.go` (add `group *livetrack.Group` field)

- [ ] **Step 1: Write the failing test for budget profiles**

`internal/daemon/lifecycle_group_test.go`:

```go
package daemon

import (
	"testing"
	"time"

	"goodkind.io/clyde/internal/livetrack"
)

// TestBudgetProfilesMatchLegacyConstants asserts the named budgets reproduce
// the pre-refactor timing exactly, so Quiesce drains on the same schedule the
// hand-sequenced path did.
func TestBudgetProfilesMatchLegacyConstants(t *testing.T) {
	if budgetReload != (livetrack.Budget{Cap: 60 * time.Second, IdleGrace: 5 * time.Second}) {
		t.Errorf("budgetReload = %+v", budgetReload)
	}
	if budgetShutdown != (livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0}) {
		t.Errorf("budgetShutdown = %+v", budgetShutdown)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/daemon/ -run TestBudgetProfilesMatchLegacyConstants -v`
Expected: FAIL with `undefined: budgetReload`.

- [ ] **Step 3: Implement the group assembly and budgets**

`internal/daemon/lifecycle_group.go`:

```go
package daemon

import (
	"context"
	"log/slog"
	"time"

	"goodkind.io/clyde/internal/livetrack"
)

// budgetReload and budgetShutdown replace the scattered reloadDrainCap,
// reloadDrainIdleGrace, and daemonShutdownTimeout constants. The values are
// identical so Quiesce drains on the same schedule the hand-sequenced path
// used.
var (
	budgetReload   = livetrack.Budget{Cap: 60 * time.Second, IdleGrace: 5 * time.Second}
	budgetShutdown = livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0}
)

// reloadQuietWait bounds the watcher's pre-reload quiet wait. A reload defers
// at most this long for in-flight client exchanges before proceeding to the
// drain. Held next to the budgets so all reload timing lives in one file.
const reloadQuietWait = 30 * time.Second

// newLifecycleGroup constructs the daemon's lifecycle group. Subsystems Attach
// their registries to it at construction and AddHook their non-registry steps;
// the daemon never hand-sequences drain order again.
func newLifecycleGroup(log *slog.Logger) *livetrack.Group {
	return livetrack.NewGroup(livetrack.GroupOptions{Log: log})
}

// awaitDaemonQuiet waits until no client-exchange surface has an active
// session within budgetReload.IdleGrace, bounded by reloadQuietWait. The
// watcher calls it before triggering reload or rebind.
func awaitDaemonQuiet(ctx context.Context, group *livetrack.Group) bool {
	waitCtx, cancel := context.WithTimeout(ctx, reloadQuietWait)
	defer cancel()
	return group.AwaitQuiet(waitCtx, budgetReload.IdleGrace)
}
```

- [ ] **Step 4: Add the group field to runtimeServices**

In `internal/daemon/runtime.go`, add to the `runtimeServices` struct after `configWatcher`:

```go
	// group is the single lifecycle plan every long-lived registry attaches
	// to. Quiesce drives reload and shutdown drains; AwaitQuiet backs the
	// watcher's quiet-wait. Constructed in startRuntime before any subsystem.
	group *livetrack.Group
```

In `startRuntime`, construct it first (`group := newLifecycleGroup(log)`), add `group: group,` to the `runtimeServices{...}` literal, and pass `group` (via `runtime.group`) into `startMITM`, `startAdapter`, and the search-manager construction. Their signatures gain the group in the following tasks.

- [ ] **Step 5: Run the budget test to verify it passes**

Run: `go test ./internal/daemon/ -run TestBudgetProfilesMatchLegacyConstants -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/lifecycle_group.go internal/daemon/lifecycle_group_test.go internal/daemon/runtime.go
git commit -m "Add daemon lifecycle group assembly and budget profiles"
```

### Task 3.2: Migrate MITM tunnels registry to Attach

**Files:**
- Modify: `internal/mitm/proxy.go` (NewProxy signature and Attach)
- Modify: `internal/daemon/runtime.go` (pass group into startMITMListener)

- [ ] **Step 1: Write the failing test asserting NewProxy takes a group**

In `internal/mitm/proxy_group_test.go`:

```go
// TestNewProxyAttachesToGroup asserts the proxy's tunnel registry is a member
// of the supplied lifecycle group, so the daemon's Quiesce drains it without
// the proxy exposing a separate Shutdown path.
func TestNewProxyAttachesToGroup(t *testing.T) {
	g := livetrack.NewGroup(livetrack.GroupOptions{Log: testLogger(t)})
	before := g.MemberCount()
	ln := newLocalListener(t)
	p, err := NewProxy(testMITMConfig(t), config.LoggingRequest{}, testLogger(t), []net.Listener{ln}, nil, "cli.test", g)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	if p.Tunnels == nil {
		t.Fatal("proxy tunnels registry is nil")
	}
	if g.MemberCount() != before+1 {
		t.Fatalf("expected 1 attached member, got %d", g.MemberCount()-before)
	}
}
```

Add `newLocalListener`, `testMITMConfig`, `testLogger` helpers if the package lacks them.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/mitm/ -run TestNewProxyAttachesToGroup -v`
Expected: FAIL with too many arguments to `NewProxy`.

- [ ] **Step 3: Change NewProxy to Attach the registry**

In `internal/mitm/proxy.go`, add `group *livetrack.Group` to the `NewProxy` signature and replace the `livetrack.New[TunnelMeta](...)` call with:

```go
		Tunnels: livetrack.Attach[TunnelMeta](group, livetrack.MemberSpec{
			Phase:         livetrack.PhaseIngress,
			QuietRelevant: true,
		}, livetrack.Options[TunnelMeta]{
			Component:     "mitm",
			Concern:       slogger.ConcernProviderMITMLifecycle,
			Log:           log,
			PollEvery:     50 * time.Millisecond,
			CloserGrace:   2 * time.Second,
			ParallelClose: false,
			Now:           nil,
		}),
```

- [ ] **Step 4: Thread the group through the daemon caller**

In `internal/daemon/runtime.go`, add `group *livetrack.Group` to `startMITM` and `startMITMListener` and pass `runtime.group` into `mitm.NewProxy`.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/mitm/ -run TestNewProxyAttachesToGroup -v`
Expected: PASS.

- [ ] **Step 6: Run the MITM and daemon packages**

Run: `go build ./... && go test ./internal/mitm/ ./internal/daemon/ -count=1`
Expected: build clean, tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/mitm/proxy.go internal/daemon/runtime.go internal/mitm/proxy_group_test.go internal/livetrack/group.go
git commit -m "Attach MITM tunnel registry to lifecycle group"
```

### Task 3.3: Migrate adapter ingress and egress registries to Attach

**Files:**
- Modify: `internal/adapter/server.go`, `internal/adapter/livetrack_meta.go`, `internal/adapter/livetrack_meta_egress.go`
- Modify: `internal/daemon/runtime.go` (pass group into startAdapter)

- [ ] **Step 1: Write the failing test asserting adapter registries attach**

In `internal/adapter/server_test.go`:

```go
// TestAdapterAttachesIngressAndEgress asserts adapter.New attaches both the
// ingress and egress registries to the supplied group as PhaseIngress and
// PhaseEgress, QuietRelevant members.
func TestAdapterAttachesIngressAndEgress(t *testing.T) {
	g := livetrack.NewGroup(livetrack.GroupOptions{Log: testLogger(t)})
	before := g.MemberCount()
	_, err := adapter.New(context.Background(), testAdapterConfig(t), testLoggingConfig(t), testDeps(t, g), testLogger(t))
	if err != nil {
		t.Fatalf("adapter.New: %v", err)
	}
	if g.MemberCount() != before+2 {
		t.Fatalf("expected 2 attached members, got %d", g.MemberCount()-before)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/adapter/ -run TestAdapterAttachesIngressAndEgress -v`
Expected: FAIL because `testDeps` has no group field and registries build via `New`.

- [ ] **Step 3: Add the group to adapter.Deps and Attach both registries**

In `internal/adapter`, add a `Group *livetrack.Group` field to `Deps`. Change the ingress registry construction in `livetrack_meta.go` and egress in `livetrack_meta_egress.go` from `livetrack.New[...]` to `livetrack.Attach[...](deps.Group, MemberSpec{...}, Options{...})`:

- ingress: `MemberSpec{Phase: PhaseIngress, QuietRelevant: true}`
- egress: `MemberSpec{Phase: PhaseEgress, QuietRelevant: true}`

Thread `deps.Group` to wherever those constructors are called inside `adapter.New`.

- [ ] **Step 4: Pass the group from the daemon**

In `internal/daemon/runtime.go`, set `deps.Group = runtime.group` inside `startAdapter` before `adapter.New`.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/adapter/ -run TestAdapterAttachesIngressAndEgress -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/adapter/ internal/daemon/runtime.go
git commit -m "Attach adapter ingress and egress registries to lifecycle group"
```

### Task 3.4: Migrate codex websocket session registry to Attach

**Files:**
- Modify: `internal/adapter/codex/livetrack_meta_ws.go` and the provider wiring that constructs it.

- [ ] **Step 1: Write the failing test**

Add a codex provider test asserting the ws session registry is attached as `PhaseEgress, QuietRelevant: true`. Mirror Task 3.3's structure against the codex provider constructor (`ProviderOptions` carries the registry today).

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/adapter/codex/ -run Attach -v`
Expected: FAIL.

- [ ] **Step 3: Thread the group into ProviderOptions and Attach**

Add `Group *livetrack.Group` to the codex `ProviderOptions`, change the `livetrack.New[WsSessionMeta]` call to `Attach` with `MemberSpec{Phase: PhaseEgress, QuietRelevant: true}`, and set the group from `adapter.New` (which has `deps.Group`).

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/adapter/codex/ -run Attach -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/codex/ internal/adapter/
git commit -m "Attach codex websocket session registry to lifecycle group"
```

### Task 3.5: Migrate search jobs registry to Attach (restartable worker)

**Files:**
- Modify: `internal/conversation/search_jobs.go`
- Modify: `internal/daemon/runtime.go` (pass group into NewSearchJobManager)

- [ ] **Step 1: Write the failing test**

In `internal/conversation/search_jobs_test.go`:

```go
// TestSearchManagerAttachesAsWorker asserts the search job registry attaches
// as a PhaseWorkers member that is NOT quiet-relevant, so background jobs
// never hold a reload to its max-wait.
func TestSearchManagerAttachesAsWorker(t *testing.T) {
	g := livetrack.NewGroup(livetrack.GroupOptions{Log: testLogger(t)})
	before := g.MemberCount()
	_ = NewSearchJobManager(testIndex(t), testStore(t), config.SearchConfig{}, testLogger(t), nil, "", g)
	if g.MemberCount() != before+1 {
		t.Fatalf("expected 1 worker member attached, got %d", g.MemberCount()-before)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/conversation/ -run TestSearchManagerAttachesAsWorker -v`
Expected: FAIL with `NewSearchJobManager` arity mismatch.

- [ ] **Step 3: Add group param and Attach as worker**

Change `NewSearchJobManager` to take a trailing `group *livetrack.Group` and replace `livetrack.New[SearchJobMeta]` with `Attach` using `MemberSpec{Phase: PhaseWorkers, QuietRelevant: false}`. Update the daemon caller in `run.go`/`runtime.go` to pass `runtime.group`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/conversation/ -run TestSearchManagerAttachesAsWorker -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/conversation/search_jobs.go internal/daemon/run.go internal/daemon/runtime.go
git commit -m "Attach search job registry to lifecycle group as worker member"
```

### Task 3.6: Migrate config watcher registry to Attach (cancel-no-wait)

**Files:**
- Modify: `internal/daemon/config_watcher.go`

- [ ] **Step 1: Write the failing test**

In `internal/daemon/config_watcher_test.go`:

```go
// TestConfigWatcherAttachesCancelNoWait asserts the watcher registry attaches
// as a PhaseWorkers, CancelNoWait member, preserving the non-blocking cancel
// that prevents a reload-triggering watcher from deadlocking on its own drain.
func TestConfigWatcherAttachesCancelNoWait(t *testing.T) {
	g := livetrack.NewGroup(livetrack.GroupOptions{Log: testLogger(t)})
	before := g.MemberCount()
	_ = newConfigWatcher(testLogger(t), "baseline", testRuntime(t, g))
	if g.MemberCount() != before+1 {
		t.Fatalf("expected watcher member attached, got %d", g.MemberCount()-before)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/daemon/ -run TestConfigWatcherAttachesCancelNoWait -v`
Expected: FAIL.

- [ ] **Step 3: Attach the watcher registry**

In `newConfigWatcher`, replace `livetrack.New[configWatcherMeta]` with `Attach(runtime.group, MemberSpec{Phase: PhaseWorkers, QuietRelevant: false, CancelNoWait: true}, Options{...})`. The watcher already takes `runtime *runtimeServices`, so `runtime.group` is in scope. Keep the existing `cancelLoop`/`stop` behavior; the `CancelNoWait` spec is what the group consults when deciding whether to wait for natural release on the reload path.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/daemon/ -run TestConfigWatcherAttachesCancelNoWait -v`
Expected: PASS.

- [ ] **Step 5: Run the whole build and test suite**

Run: `go build ./... && go test ./... -count=1`
Expected: build clean; all tests PASS (registries now construct via Attach; old `New`/`Drain` still exist and back them).

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/config_watcher.go
git commit -m "Attach config watcher registry to lifecycle group"
```

---

## Phase 4: Flip the choke points and delete the parallel paths

### Task 4.1: Unexport `New`, `Drain`, `DrainWith`; add the compile-surface test

**Files:**
- Modify: `internal/livetrack/registry.go`, `internal/livetrack/drain.go`, `internal/livetrack/group.go`, `internal/livetrack/group_member.go`
- Create: `internal/livetrack/group_choke_test.go`

- [ ] **Step 1: Rename the exported entry points to unexported**

In `registry.go`: rename `func New[M Meta]` to `func newRegistry[M Meta]`. In `drain.go`: rename `func (r *Registry[M]) Drain` to `drain` and `DrainWith` to `drainWith`. Update `Attach` to call `newRegistry`, and `registryMember.drainWith` to call `r.reg.drainWith`. The semantics are identical; only the names change. Update the internal tests that called `New`/`Register` directly (Tasks 2.2, 2.4) to the new names.

- [ ] **Step 2: Update every caller**

Run: `go build ./...`
Expected: build errors at any remaining external `livetrack.New(`, `.Drain(`, or `.DrainWith(` call. Fix each by routing through `Attach` or `group.Quiesce`. Iterate until clean. Any remaining direct call is a real violation, not a rename target.

- [ ] **Step 3: Write the compile-surface choke test**

`internal/livetrack/group_choke_test.go`:

```go
package livetrack

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestNoExportedConstructorOrDrain asserts the package no longer exports New,
// Drain, or DrainWith. The only way to obtain a registry is Attach, and the
// only way to drain is the group. This is the structural choke point: a
// registry outside the group cannot be constructed, enforced at the API level
// rather than by a launderable runtime test.
func TestNoExportedConstructorOrDrain(t *testing.T) {
	fset := token.NewFileSet()
	forbidden := map[string]bool{"New": true, "Drain": true, "DrainWith": true}
	for _, file := range parsePackageFiles(t, fset, ".") {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if forbidden[fn.Name.Name] && fn.Name.IsExported() {
				t.Errorf("forbidden exported lifecycle entry point still present: %s", fn.Name.Name)
			}
		}
	}
}

// parsePackageFiles parses every non-test .go file in dir.
func parsePackageFiles(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	matches, _ := filepathGlobGo(dir)
	files := make([]*ast.File, 0, len(matches))
	for _, path := range matches {
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, f)
	}
	return files
}
```

Implement `filepathGlobGo(dir)` to return `*.go` files excluding `_test.go`, mirroring the walk in `livetrack_lints_test.go`.

- [ ] **Step 4: Run the choke test**

Run: `go test ./internal/livetrack/ -run TestNoExportedConstructorOrDrain -v`
Expected: PASS (after the renames).

- [ ] **Step 5: Run the full suite**

Run: `go build ./... && go test ./... -count=1`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add internal/livetrack/
git commit -m "Unexport livetrack New, Drain, DrainWith behind Attach and Group choke points"
```

### Task 4.2: Replace `startReloadDrain` with `group.Quiesce` and decompose adapter/MITM shutdown into hooks

**Files:**
- Modify: `internal/daemon/reload.go`, `internal/daemon/lifecycle_group.go`
- Modify: `internal/adapter/server_routes.go`, `internal/adapter/server.go`, `internal/mitm/proxy.go`

- [ ] **Step 1: Move adapter non-registry steps into group hooks**

The adapter's `ShutdownWith` does: keepalives off, ingress drain, `httpSrv.Shutdown`, egress drain, codex ws drain. Registries now drain via the group (ingress at PhaseIngress, egress and ws at PhaseEgress). The non-registry steps become hooks added when the adapter attaches. In adapter construction, after attaching registries, register hooks on `deps.Group`:

```go
deps.Group.AddHook(livetrack.PhaseIngress, "adapter.keepalives_off", func(ctx context.Context) error {
	if s.httpSrv != nil {
		s.httpSrv.SetKeepAlivesEnabled(false)
	}
	return nil
})
deps.Group.AddHook(livetrack.PhaseEgress, "adapter.http_shutdown", func(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
})
```

Ordering requirement: today keepalives-off and ingress drain precede `httpSrv.Shutdown`, which precedes egress drain. Phase order gives PhaseIngress (keepalives-off hook plus ingress registry drain), then PhaseEgress (http_shutdown hook plus egress and ws registry drains). The current `quiescePhase` runs member drains, then hooks. The adapter needs `httpSrv.Shutdown` to run after ingress drains but before egress drains, which PhaseEgress satisfies only if the hook runs before that phase's member drains. Resolve this with the failing characterization test from Task 1.1, not by guessing: if member-drain-then-hook ordering conflicts, add a pre-member hook slot per phase (run hooks before member drains for that phase) and document why. Decide via the green characterization test.

- [ ] **Step 2: Move MITM `server.Shutdown` into a PhaseIngress hook**

MITM tunnels are PhaseIngress members. Each proxy's `server.Shutdown` becomes a PhaseIngress hook added at `NewProxy` time, so each proxy contributes its own hook.

- [ ] **Step 3: Move store closes into PhaseStorage hooks**

In `startRuntime`, after stores are constructed, add PhaseStorage hooks for the search store close, semantic runtime close, and capture store close, in that order. Search jobs are a PhaseWorkers member, so their drain happens in the workers phase; the post-drain store close is the storage hook. This reproduces `closeStoresOnReload`.

- [ ] **Step 4: Replace startReloadDrain body with Quiesce**

In `reload.go`, change `startReloadDrain` to:

```go
func (r *runtimeServices) startReloadDrain(parent context.Context, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(parent, "daemon.reload.public_drain_panic", "concern", "daemon.workers.reload", "component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		r.group.Quiesce(parent, "reload", budgetReload)
	}()
	return done
}
```

Delete `closeStoresOnReload` (its steps are now hooks) and drop the now-unused `sync` and `livetrack.DrainOptions` references if they fall out. Keep `waitReloadDrain` and `activeSurfaceCount`.

- [ ] **Step 5: Delete the dead adapter and MITM shutdown methods**

After Quiesce takes over, remove the methods that no longer have callers: the standalone `Server.WaitForIdle` ticker at `server_routes.go:227`, and the `ShutdownWith`/`Shutdown` wrappers that only the drain path called. Keep `Server.ForceCloseAll`/`Close` only if the deadline path still needs them; the group's `drainWith` already force-closes on the deadline. Find dead references:

Run: `go build ./... && make deadcode`
Expected: build clean; `deadcode` reports nothing newly dead, or you delete what it flags. Fix each honestly (no `//nolint`, no allowlist edits).

- [ ] **Step 6: Run the characterization tests**

Run: `go test ./internal/daemon/ -run TestReloadDrain -v`
Expected: PASS. Drain order and budgets stay bit-for-bit. If order differs, adjust hook phase placement (Step 1) until green.

- [ ] **Step 7: Run the full suite**

Run: `go build ./... && go test ./... -count=1`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/daemon/ internal/adapter/ internal/mitm/
git commit -m "Replace hand-sequenced reload drain with group.Quiesce and phase hooks"
```

### Task 4.3: Replace `runtimeServices.shutdown` hand-sequence with `group.Quiesce`

**Files:**
- Modify: `internal/daemon/runtime.go`

- [ ] **Step 1: Write the failing test**

```go
// TestShutdownUsesGroupQuiesce asserts shutdown drives the lifecycle group at
// the shutdown budget rather than the deleted hand-sequence, and still closes
// listeners. Use a runtime with stub members recording their drain.
func TestShutdownUsesGroupQuiesce(t *testing.T) {
	rec := newOrderRecorder()
	r := newRecordingRuntime(t, rec)
	r.shutdown(testContext(t))
	rec.assertBefore(t, "adapter", "capture_store")
}
```

- [ ] **Step 2: Run it to verify it drives the change**

Run: `go test ./internal/daemon/ -run TestShutdownUsesGroupQuiesce -v`
Expected: FAIL until `shutdown` is rewritten.

- [ ] **Step 3: Rewrite shutdown to call Quiesce**

Replace the per-subsystem body of `runtimeServices.shutdown` (at `runtime.go:417`) with: stop the config watcher (non-blocking cancel, as today), close the daemon and pprof listeners, then `r.group.Quiesce(parent, "shutdown", budgetShutdown)`. The adapter, MITM, and store closes that were inline are now group members and PhaseStorage hooks, so Quiesce performs them in order. Keep listener closes outside the group (they are not registries).

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/daemon/ -run TestShutdownUsesGroupQuiesce -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/runtime.go
git commit -m "Drive daemon shutdown through group.Quiesce"
```

### Task 4.4: Register the capture store as a PhaseStorage member (fix the standing flock gap)

**Files:**
- Modify: `internal/mitm/capture/store.go` (or wherever the store and its flock live), `internal/daemon/runtime.go`

- [ ] **Step 1: Write the failing test**

```go
// TestCaptureStoreRegistersGroupMember asserts the capture store, whose
// cross-process flock AGENTS.md requires to be tracked, is a declared
// storage-phase member or named storage hook rather than a hand-closed
// trailing call.
func TestCaptureStoreRegistersGroupMember(t *testing.T) {
	// Construct a runtime with a real (temp) capture store + group, assert the
	// group has a storage-phase hook or member named for the capture store.
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/daemon/ -run TestCaptureStoreRegistersGroupMember -v`
Expected: FAIL.

- [ ] **Step 3: Make the capture store a storage member or named storage hook**

Minimal faithful fix: register the capture store close as a named PhaseStorage hook (`"capture_store.close"`) at construction, and if the store holds a flock as a long-lived session, attach a `PhaseStorage` registry session for the flock so force-close releases it. Reproduce the close-after-surfaces ordering.

- [ ] **Step 4: Run the test and the package**

Run: `go test ./internal/daemon/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mitm/capture/ internal/daemon/runtime.go
git commit -m "Register capture store as storage-phase lifecycle member"
```

### Task 4.5: Extend the livetrack lint to enforce group-only construction

**Files:**
- Modify: `internal/livetrack/livetrack_lints_test.go`

- [ ] **Step 1: Add the membership assertion**

`New` is now unexported, so the membership guarantee is mostly compile-enforced. Add the runtime half: assert there are no direct `newRegistry[` calls outside `Attach`. Walk the package files and fail if `newRegistry` appears anywhere except `Attach`'s body.

```go
// TestRegistryConstructionOnlyViaAttach asserts newRegistry is called only
// inside Attach, so the group-membership invariant cannot be bypassed even
// within the package.
func TestRegistryConstructionOnlyViaAttach(t *testing.T) {
	// parse group.go; confirm the only newRegistry call site is the Attach func.
}
```

- [ ] **Step 2: Run it**

Run: `go test ./internal/livetrack/ -run TestRegistryConstructionOnlyViaAttach -v`
Expected: PASS.

- [ ] **Step 3: Keep the Shutdown-pairing guard working**

`TestLivetrackAdoptersPairShutdownWithDrain` guarded against bare `http.Server.Shutdown`. Those calls now live in group hook closures. Keep the test. If the hook closures defeat the AST heuristic, update the heuristic to accept a `Quiesce` or `AddHook` reference in the same package as the pairing signal. Do not delete the guard.

- [ ] **Step 4: Run the full livetrack suite**

Run: `go test ./internal/livetrack/ -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/livetrack/livetrack_lints_test.go
git commit -m "Extend livetrack lint to enforce group-only registry construction"
```

### Task 4.6: Move timing constants out of run.go into the budget profiles

**Files:**
- Modify: `internal/daemon/run.go`, `internal/daemon/lifecycle_group.go`, `internal/daemon/reload_behavior_test.go`

- [ ] **Step 1: Delete the migrated constants from run.go**

Remove `reloadDrainCap`, `reloadDrainIdleGrace`, and `daemonShutdownTimeout` from `run.go:33-35` once every reference uses `budgetReload`/`budgetShutdown`. Update `waitReloadDrain`'s log line that referenced `reloadDrainCap.String()` to `budgetReload.Cap.String()`. Update Task 1.1's `TestReloadDrainBudgetsAreStable` to assert the budget values instead of the deleted constants; the values are unchanged, only the home moves.

- [ ] **Step 2: Run the build and budget test**

Run: `go build ./... && go test ./internal/daemon/ -run Budget -v`
Expected: clean, PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/daemon/run.go internal/daemon/lifecycle_group.go internal/daemon/reload_behavior_test.go
git commit -m "Consolidate reload timing constants into budget profiles"
```

### Task 4.7: Full gate run after the refactor

- [ ] **Step 1: Run every make gate**

Run: `make test lint fmt staticcheck staticcheck-extra deadcode audit build`
Expected: all green.

- [ ] **Step 2: Confirm fmt left no diff**

Run: `make fmt && git diff --exit-code`
Expected: exit 0, no diff.

- [ ] **Step 3: Commit any fmt-only changes if present**

```bash
git add -A && git commit -m "Apply gofmt after lifecycle group refactor" || true
```

---

## Phase 5: Feature 1, quiet-wait before reload and rebind

### Task 5.1: Gate the watcher's reload/rebind trigger behind AwaitQuiet

**Files:**
- Modify: `internal/daemon/config_watcher.go`

- [ ] **Step 1: Write the failing tests**

In `config_watcher_test.go`:

```go
// TestHandleChangeWaitsForQuietThenTriggers asserts handleChange calls the
// quiet wait before triggering, and that it re-hashes after the wait so a
// reverted edit during the wait skips the reload.
func TestHandleChangeWaitsForQuietThenTriggers(t *testing.T) {
	// build a watcher with injected quietWait that blocks until a signal, a
	// parse stub returning ok, and a reload stub recording calls.
	// Assert: reload not called until quiet signalled; called once after.
}

// TestHandleChangeSkipsReloadWhenRevertedDuringWait asserts that if the file
// hash returns to baseline while waiting for quiet, no reload fires.
func TestHandleChangeSkipsReloadWhenRevertedDuringWait(t *testing.T) {
	// hash returns changed first, baseline after the wait; assert no trigger.
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/daemon/ -run 'TestHandleChange(WaitsForQuiet|SkipsReload)' -v`
Expected: FAIL.

- [ ] **Step 3: Add the quiet-wait to handleChange**

In `config_watcher.go`, add a `group *livetrack.Group` field set in `newConfigWatcher` (from `runtime.group`) and a `quietWait func(context.Context) bool` field defaulting to `func(ctx context.Context) bool { return awaitDaemonQuiet(ctx, w.group) }` so tests can stub it. In `handleChange`, after the parse validation succeeds and the route is reload or rebind, insert:

```go
	// Wait for in-flight client exchanges to finish before severing them.
	// awaitDaemonQuiet polls quiet-relevant members (adapter ingress/egress,
	// codex ws, MITM tunnels) within the idle grace, bounded by reloadQuietWait.
	w.quietWait(ctx)
	// Re-hash: a revert during the wait means there is nothing to apply.
	postHash, ok := configFileHash(ctx, w.log, w.path)
	if !ok || postHash == w.baselineHash {
		w.log.InfoContext(ctx, "daemon.config_watch.reverted_during_wait", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon")
		return false
	}
```

Place this between the `change_detected` log and the `listenerChanged` routing.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/daemon/ -run 'TestHandleChange(WaitsForQuiet|SkipsReload)' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/config_watcher.go
git commit -m "Defer watcher reload and rebind until client exchanges are quiet"
```

### Task 5.2: Live validation, quiet-wait, bound, idle-vs-active

**Files:**
- Modify: `test/live/reload_live_test.go`

- [ ] **Step 1: Add the live quiet-wait scenarios**

Add live tests (tag `live`) using the harness: boot the worktree daemon on fake ports with a fake config, then:

```go
// TestLiveQuietWaitDefersUntilRequestFinishes boots the worktree daemon,
// starts a slow in-flight adapter request, edits a hot field, and asserts the
// reload defers until the request finishes (observed via daemon.config_watch.*
// and livetrack logs in the temp state root), then applies.
func TestLiveQuietWaitDefersUntilRequestFinishes(t *testing.T) { /* boot, request, edit, assert order, cleanup */ }

// TestLiveQuietWaitBoundFiresCap asserts a request longer than reloadQuietWait
// still takes the drain: the cap fires and the reload proceeds.
func TestLiveQuietWaitBoundFiresCap(t *testing.T) { /* slow request > cap, assert drain fired */ }

// TestLiveIdleKeepaliveDoesNotBlockQuiet opens an idle keepalive connection
// (no in-flight request), edits a hot field, and asserts the reload does not
// wait on the parked connection.
func TestLiveIdleKeepaliveDoesNotBlockQuiet(t *testing.T) { /* idle conn, edit, assert no wait */ }
```

Each test reads the temp-state JSONL logs (daemon log plus the config concern log for `daemon.config_watch.*` and livetrack drain events) to assert ordering, and calls `h.assertProductionUntouched(t)` in a `t.Cleanup`.

- [ ] **Step 2: Run the live quiet-wait scenarios**

Run: `go test -tags live ./test/live/ -run TestLiveQuietWait -v`
Expected: PASS. Preflight runs first; if a fake port is occupied, the suite fails fast per the contract.

- [ ] **Step 3: Commit**

```bash
git add test/live/reload_live_test.go
git commit -m "Add live quiet-wait, cap-bound, and idle-keepalive scenarios"
```

---

## Phase 6: Feature 2, in-process config apply

### Task 6.1: `ClassifyConfigChange` typed routing

**Files:**
- Create: `internal/config/change_class.go`
- Create: `internal/config/change_class_test.go`

- [ ] **Step 1: Write the failing classifier tests**

`internal/config/change_class_test.go`:

```go
package config

import "testing"

// TestClassifyConfigChange asserts a port change routes to rebind, an adapter
// enable/disable to restart-required, a capture-db-path change to reload, and a
// model-alias-only edit to hot-apply. Unknown or unclassified field changes
// must route to reload, never hot-apply.
func TestClassifyConfigChange(t *testing.T) {
	base := minimalConfig()
	cases := []struct {
		name   string
		mutate func(*Config)
		want   Route
	}{
		{"adapter port", func(c *Config) { c.Adapter.Port = 21434 }, RouteRebind},
		{"cursor ingress port", func(c *Config) { c.Adapter.CursorIngressPort = 21435 }, RouteRebind},
		{"adapter disabled", func(c *Config) { c.Adapter.Enabled = false }, RouteRestartRequired},
		{"pprof addr", func(c *Config) { c.Debug.PProfAddr = "[::1]:0" }, RouteRebind},
		{"capture db path", func(c *Config) { c.MITM.CaptureStore.DBPath = "/tmp/x.db" }, RouteReload},
		{"model alias added", func(c *Config) { c.Adapter.Models = map[string]AdapterModel{"x": {Backend: "claude"}} }, RouteHotApply},
		{"notices toggled", func(c *Config) { f := false; c.Adapter.Notices.Enabled = &f }, RouteHotApply},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := base // value copy
			tc.mutate(&next)
			if got := ClassifyConfigChange(base, next); got != tc.want {
				t.Errorf("ClassifyConfigChange = %v, want %v", got, tc.want)
			}
		})
	}
}
```

Add a `minimalConfig()` helper returning a valid baseline `Config` with the adapter enabled.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/config/ -run TestClassifyConfigChange -v`
Expected: FAIL with `undefined: Route`.

- [ ] **Step 3: Implement the classifier**

`internal/config/change_class.go`:

```go
package config

// Route is the lifecycle decision for one config change. It is consumed by the
// watcher, the reload validation, and the in-process apply path so the
// reload-vs-rebind-vs-hot-apply decision lives in one place.
type Route uint8

const (
	// RouteHotApply means every changed field is safe to apply in process.
	RouteHotApply Route = iota
	// RouteReload means a zero-bind-gap re-exec is required (state the new
	// generation must reopen, e.g. capture db path), but listeners are
	// unchanged.
	RouteReload
	// RouteRebind means a listener address moved; ports must be rebound.
	RouteRebind
	// RouteRestartRequired means a change the running generation cannot honor
	// without a full restart (e.g. enabling or disabling a whole surface).
	RouteRestartRequired
)

// String returns a stable label for logs.
func (r Route) String() string {
	switch r {
	case RouteHotApply:
		return "hot_apply"
	case RouteReload:
		return "reload"
	case RouteRebind:
		return "rebind"
	case RouteRestartRequired:
		return "restart_required"
	default:
		return "unknown"
	}
}

// ClassifyConfigChange compares old and new config and returns the most
// disruptive route any changed field requires. The classifier is conservative:
// a changed field that is not explicitly classified hot-appliable routes to
// reload, never silently no-op. The escalation order is
// RestartRequired > Rebind > Reload > HotApply.
func ClassifyConfigChange(oldCfg, newCfg Config) Route {
	if oldCfg.Adapter.Enabled != newCfg.Adapter.Enabled {
		return RouteRestartRequired
	}
	if oldCfg.MITM.EnabledDefault != newCfg.MITM.EnabledDefault {
		return RouteRestartRequired
	}
	if listenerTopologyMoved(oldCfg, newCfg) {
		return RouteRebind
	}
	if reloadOnlyStateChanged(oldCfg, newCfg) {
		return RouteReload
	}
	if nonHotFieldChanged(oldCfg, newCfg) {
		return RouteReload
	}
	return RouteHotApply
}
```

Implement three helpers in the same file:
- `listenerTopologyMoved`: adapter host/port/cursor port, every MITM listener host/port, pprof addr.
- `reloadOnlyStateChanged`: capture store db path, CA cert/key paths, logging sink layout.
- `nonHotFieldChanged`: the conservative default. Project both configs down to only the known hot-appliable fields, blank those fields out on copies of both configs, and report whether anything else differs (`!reflect.DeepEqual` of the blanked copies). This guarantees an unclassified field change routes to reload rather than being silently dropped. Document the hot set in the function comment and keep it in sync with the spec's v1 list.

- [ ] **Step 4: Run the classifier tests to verify they pass**

Run: `go test ./internal/config/ -run TestClassifyConfigChange -v`
Expected: PASS.

- [ ] **Step 5: Add a conservative-default test**

```go
// TestClassifyUnknownFieldRoutesToReload asserts a change to a field outside
// the hot set (here, a logging level) does not hot-apply; the conservative
// default routes it to reload.
func TestClassifyUnknownFieldRoutesToReload(t *testing.T) {
	base := minimalConfig()
	next := base
	next.Logging /* some non-hot field */ = /* changed value */
	if got := ClassifyConfigChange(base, next); got == RouteHotApply {
		t.Fatal("unclassified field change must not hot-apply")
	}
}
```

Pick a concrete non-hot logging field for the mutation.

- [ ] **Step 6: Run and commit**

Run: `go test ./internal/config/ -run TestClassify -v`
Expected: PASS.

```bash
git add internal/config/change_class.go internal/config/change_class_test.go
git commit -m "Add ClassifyConfigChange typed reload routing"
```

### Task 6.2: Adapter `ConfigSnapshot` holder and atomic swap

**Files:**
- Create: `internal/adapter/runtime_snapshot.go`
- Create: `internal/adapter/runtime_snapshot_test.go`

- [ ] **Step 1: Write the failing snapshot swap test**

`internal/adapter/runtime_snapshot_test.go`:

```go
package adapter

import (
	"sync"
	"testing"
)

// TestConfigSnapshotSwapIsAtomic asserts a request that loads the snapshot at
// ingress keeps using it across a concurrent swap, while a request that starts
// after the swap sees the new one. This is the graceful-handoff property:
// in-flight exchanges finish on the config they started with.
func TestConfigSnapshotSwapIsAtomic(t *testing.T) {
	h := newSnapshotHolder(testSnapshot("v1"))
	got1 := h.Load()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.Store(testSnapshot("v2"))
	}()
	wg.Wait()
	if got1.version != "v1" {
		t.Errorf("held snapshot mutated under request: %s", got1.version)
	}
	if h.Load().version != "v2" {
		t.Errorf("new load did not see swapped snapshot")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/adapter/ -run TestConfigSnapshotSwapIsAtomic -v`
Expected: FAIL with `undefined: newSnapshotHolder`.

- [ ] **Step 3: Implement the snapshot holder**

`internal/adapter/runtime_snapshot.go`:

```go
package adapter

import "sync/atomic"

// ConfigSnapshot is the immutable bundle of everything the adapter builds from
// config: model registry, resolver, client identity, reasoning levers, retry
// policies, notices, auth deps, wire baseline paths, passthrough tables, and
// pricing. A request loads one snapshot at ingress and uses it throughout, so
// an in-flight exchange finishes on the config it started with while a config
// apply swaps the holder for the next request. The version field is a debug
// label; Task 6.3 fills the real config-derived fields.
type ConfigSnapshot struct {
	version string
}

// snapshotHolder holds the current ConfigSnapshot behind an atomic pointer so
// loads are lock-free on the request hot path and swaps are atomic.
type snapshotHolder struct {
	ptr atomic.Pointer[ConfigSnapshot]
}

func newSnapshotHolder(initial *ConfigSnapshot) *snapshotHolder {
	h := &snapshotHolder{ptr: atomic.Pointer[ConfigSnapshot]{}}
	h.ptr.Store(initial)
	return h
}

// Load returns the current snapshot. Lock-free; safe on the hot path.
func (h *snapshotHolder) Load() *ConfigSnapshot { return h.ptr.Load() }

// Store swaps in a new snapshot for subsequent requests.
func (h *snapshotHolder) Store(s *ConfigSnapshot) { h.ptr.Store(s) }
```

Add a `testSnapshot(version string) *ConfigSnapshot` helper in the test file.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/adapter/ -run TestConfigSnapshotSwapIsAtomic -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/runtime_snapshot.go internal/adapter/runtime_snapshot_test.go
git commit -m "Add adapter ConfigSnapshot holder with atomic per-request swap"
```

### Task 6.3: Build the snapshot from config and load it at ingress

**Files:**
- Modify: `internal/adapter/runtime_snapshot.go`, `internal/adapter/server.go`, request handlers

- [ ] **Step 1: Write the failing test for buildConfigSnapshot**

```go
// TestBuildConfigSnapshotPopulatesRegistry asserts buildConfigSnapshot turns
// an AdapterConfig into a ConfigSnapshot whose model registry resolves a
// declared alias, proving the config-derived state is fully captured.
func TestBuildConfigSnapshotPopulatesRegistry(t *testing.T) {
	snap, err := buildConfigSnapshot(testAdapterConfig(t), testLoggingConfig(t), testDeps(t, nil))
	if err != nil {
		t.Fatalf("buildConfigSnapshot: %v", err)
	}
	if !snap.resolvesDeclaredAlias(t) {
		t.Fatal("snapshot registry missing a declared alias")
	}
}
```

Use whatever the package's real resolve entry point is for the assertion; `resolvesDeclaredAlias` is a stand-in for that call.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/adapter/ -run TestBuildConfigSnapshotPopulatesRegistry -v`
Expected: FAIL.

- [ ] **Step 3: Move config-derived construction into buildConfigSnapshot**

Identify everything `adapter.New` builds from `cfg.Adapter`, `cfg.Logging`, and `deps` that a request reads (model registry, resolver, identity, reasoning, retry, notices, auth lookups, baseline paths, passthrough, pricing). Extract that construction into `buildConfigSnapshot(cfg config.AdapterConfig, logging config.LoggingConfig, deps Deps) (*ConfigSnapshot, error)`, populate the `ConfigSnapshot` fields, and have `adapter.New` call it and store the result in a `snapshotHolder` on `Server`. Replace per-request reads of those fields with `s.snapshot.Load().<field>`. This is the largest single change; keep each moved field's behavior identical and run the adapter package tests continuously. The snapshot is a relocation, not a redesign.

- [ ] **Step 4: Run adapter tests**

Run: `go test ./internal/adapter/... -count=1`
Expected: PASS. Existing behavior preserved; requests now read through the snapshot.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/
git commit -m "Build adapter config snapshot and load it at request ingress"
```

### Task 6.4: Adapter `ApplyConfig` swaps the snapshot

**Files:**
- Modify: `internal/adapter/runtime_snapshot.go`, `internal/adapter/server.go`

- [ ] **Step 1: Write the failing test**

```go
// TestApplyConfigSwapsSnapshot asserts ApplyConfig rebuilds the snapshot and
// that a request after apply resolves an alias only present in the new config,
// with no server restart.
func TestApplyConfigSwapsSnapshot(t *testing.T) {
	s := newTestServer(t, configWithoutAliasX(t))
	if s.snapshotResolves(t, "alias-x") {
		t.Fatal("alias-x present before apply")
	}
	if err := s.ApplyConfig(configWithAliasX(t).Adapter, testLoggingConfig(t)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if !s.snapshotResolves(t, "alias-x") {
		t.Fatal("alias-x absent after apply")
	}
}
```

`snapshotResolves` is a test helper calling the real resolve path against `s.snapshot.Load()`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/adapter/ -run TestApplyConfigSwapsSnapshot -v`
Expected: FAIL with `s.ApplyConfig undefined`.

- [ ] **Step 3: Implement ApplyConfig**

```go
// ApplyConfig rebuilds the config-derived snapshot from newCfg and swaps it in
// for subsequent requests. In-flight requests keep the snapshot they loaded at
// ingress, so nothing is severed. Returns an error without swapping if the new
// config fails to build, so a bad apply leaves the running snapshot intact.
func (s *Server) ApplyConfig(newCfg config.AdapterConfig, logging config.LoggingConfig) error {
	snap, err := buildConfigSnapshot(newCfg, logging, s.deps)
	if err != nil {
		return fmt.Errorf("adapter apply config: %w", err)
	}
	s.snapshot.Store(snap)
	s.log.Info("adapter.config.applied", "concern", "adapter.http", "component", "adapter")
	return nil
}
```

Store `deps` on `Server` if it is not already held.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/adapter/ -run TestApplyConfigSwapsSnapshot -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/
git commit -m "Add adapter ApplyConfig snapshot swap with build-fail safety"
```

### Task 6.5: MITM `ApplyConfig` for routing state

**Files:**
- Modify: `internal/mitm/proxy.go`

- [ ] **Step 1: Write the failing test**

```go
// TestProxyApplyConfigSwapsRouting asserts ApplyConfig swaps the proxy's
// config-derived routing while established tunnels keep their behavior until
// they close, matching the docs/cursor.md drain contract.
func TestProxyApplyConfigSwapsRouting(t *testing.T) {
	p := newTestProxy(t, mitmConfigA(t))
	if err := p.ApplyConfig(mitmConfigB(t)); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if got := p.config().Providers; !reflect.DeepEqual(got, mitmConfigB(t).Providers) {
		t.Errorf("proxy routing not swapped: %v", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/mitm/ -run TestProxyApplyConfigSwapsRouting -v`
Expected: FAIL.

- [ ] **Step 3: Implement Proxy.ApplyConfig**

The proxy holds `cfg` behind `p.mu` (see `config()` at `proxy.go:249`). Add:

```go
// ApplyConfig swaps the proxy's config-derived state (provider routing,
// capture settings) under the existing lock. Established tunnels hold their own
// derived context and finish on the behavior they started with; only new
// requests see the swapped config. Listener topology is never changed here
// (that routes to rebind), so no socket is touched.
func (p *Proxy) ApplyConfig(newCfg config.MITMConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = newCfg
	return nil
}
```

If any per-request derived value (hooks, capture body cap) is precomputed at construction rather than read from `p.cfg` per request, recompute it here under the lock.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/mitm/ -run TestProxyApplyConfigSwapsRouting -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mitm/proxy.go
git commit -m "Add MITM Proxy.ApplyConfig for in-process routing swap"
```

### Task 6.6: Daemon `applyConfigInProcess` orchestration

**Files:**
- Create: `internal/daemon/config_apply.go`
- Create: `internal/daemon/config_apply_test.go`
- Modify: `internal/daemon/runtime.go` (hold current config pointer)

- [ ] **Step 1: Write the failing orchestration tests**

`internal/daemon/config_apply_test.go`:

```go
package daemon

import "testing"

// TestApplyConfigInProcessSwapsComponents asserts applyConfigInProcess calls
// ApplyConfig on the adapter and every MITM proxy, restarts worker members,
// updates the current-config pointer, and never re-execs.
func TestApplyConfigInProcessSwapsComponents(t *testing.T) {
	r := newApplyTestRuntime(t) // stub adapter+proxies recording ApplyConfig
	newCfg := configWithModelAlias(t, "alias-y")
	if err := r.applyConfigInProcess(testContext(t), testLogger(t), newCfg); err != nil {
		t.Fatalf("applyConfigInProcess: %v", err)
	}
	assertAdapterApplied(t, r, newCfg)
	assertEachProxyApplied(t, r, newCfg)
	if r.currentConfig.Load() != newCfg {
		t.Fatal("current config pointer not updated")
	}
}

// TestApplyConfigInProcessAbortsOnFailure asserts that if the adapter apply
// fails, the current-config pointer is not advanced and the error is returned
// so the watcher falls back to reload.
func TestApplyConfigInProcessAbortsOnFailure(t *testing.T) {
	r := newApplyTestRuntime(t)
	r.failAdapterApply = true
	before := r.currentConfig.Load()
	if err := r.applyConfigInProcess(testContext(t), testLogger(t), configWithModelAlias(t, "z")); err == nil {
		t.Fatal("expected error on adapter apply failure")
	}
	if r.currentConfig.Load() != before {
		t.Fatal("current config advanced despite apply failure")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/daemon/ -run TestApplyConfigInProcess -v`
Expected: FAIL.

- [ ] **Step 3: Add the current-config holder and implement apply**

In `runtime.go`, add `currentConfig atomic.Pointer[config.Config]` to `runtimeServices`, stored at `startRuntime` after load.

`internal/daemon/config_apply.go`:

```go
package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/config"
)

// applyConfigInProcess applies a hot-appliable config change without re-exec.
// It swaps the adapter snapshot, swaps every MITM proxy's routing, restarts
// worker members from the new config, and advances the current-config pointer.
// If any component apply fails, it returns the error WITHOUT advancing the
// pointer so the caller falls back to the quiet-wait reload route.
func (r *runtimeServices) applyConfigInProcess(ctx context.Context, log *slog.Logger, newCfg *config.Config) error {
	if r.adapter != nil {
		if err := r.adapter.ApplyConfig(newCfg.Adapter, newCfg.Logging); err != nil {
			return fmt.Errorf("apply adapter config: %w", err)
		}
	}
	for id, proxy := range r.mitmProxies {
		if err := proxy.ApplyConfig(newCfg.MITM); err != nil {
			return fmt.Errorf("apply mitm config for %q: %w", id, err)
		}
	}
	if err := r.restartWorkersForConfig(ctx, log, newCfg); err != nil {
		return fmt.Errorf("restart workers: %w", err)
	}
	r.currentConfig.Store(newCfg)
	log.InfoContext(ctx, "daemon.config.applied_in_process", "concern", "daemon.workers.reload", "component", "daemon")
	return nil
}

// restartWorkersForConfig stops the config-dependent worker loops (drift,
// semantic runtime) and restarts them from newCfg. Workers are PhaseWorkers
// group members; this uses their existing stop/start paths so the group remains
// the only lifecycle owner. The search manager re-reads cfg.Search on its next
// job, so it needs no restart here.
func (r *runtimeServices) restartWorkersForConfig(ctx context.Context, log *slog.Logger, newCfg *config.Config) error {
	// Stop and restart the periodic drift loop and semantic runtime with newCfg,
	// reusing the existing start helpers and the cancel handles held on
	// runtimeServices. Add a cancel handle to runtimeServices for any worker
	// that lacks one so the restart is clean and leaks no goroutine.
	return nil
}
```

Fill `restartWorkersForConfig` against the real worker handles in `runtime.go`/`run.go`. If a worker has no cancel handle today, add one stored on `runtimeServices` so the restart is clean.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestApplyConfigInProcess -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/config_apply.go internal/daemon/config_apply_test.go internal/daemon/runtime.go
git commit -m "Add in-process config apply orchestration with worker restart"
```

### Task 6.7: Route the watcher through ClassifyConfigChange

**Files:**
- Modify: `internal/daemon/config_watcher.go`

- [ ] **Step 1: Write the failing tests**

```go
// TestHandleChangeHotApplies asserts a hot-appliable edit calls
// applyConfigInProcess (no reload trigger), updates the baseline hash, and
// returns false so the watcher keeps looping without exiting.
func TestHandleChangeHotApplies(t *testing.T) {
	// watcher with classify stub returning RouteHotApply, apply stub recording
	// the call, reload stub asserting NOT called; assert baseline updated and
	// handleChange returns false.
}

// TestHandleChangeFallsBackToReloadOnApplyFailure asserts that when
// applyConfigInProcess errors, the watcher proceeds to the quiet-wait reload.
func TestHandleChangeFallsBackToReloadOnApplyFailure(t *testing.T) {
	// apply stub returns error; assert reload stub called once.
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/daemon/ -run 'TestHandleChange(HotApplies|FallsBack)' -v`
Expected: FAIL.

- [ ] **Step 3: Wire the classifier into handleChange**

Modify `handleChange`: after parse succeeds, classify with `config.ClassifyConfigChange(*w.currentConfig(), *parsedCfg)`. Branch:

- `RouteHotApply`: call `w.applyInProcess(ctx, parsedCfg)`. On success, update `w.baselineHash` to the new file hash and return false (keep looping, no process replacement). On error, log and fall through to the reload path.
- `RouteReload`: existing reload path, now preceded by the quiet-wait from Task 5.1.
- `RouteRebind`: existing rebind path, preceded by quiet-wait.
- `RouteRestartRequired`: log `restart_required` and return false; the operator must restart, so do not re-exec into a config the generation cannot honor.

Inject `applyInProcess func(context.Context, *config.Config) error` and `currentConfig func() *config.Config` as watcher fields, defaulting to the runtime methods, so tests stub them. Replace the old `listenerChanged` boolean routing with the `Route` so there is one decision, matching the spec's single typed decision function.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/daemon/ -run 'TestHandleChange(HotApplies|FallsBack)' -v`
Expected: PASS.

- [ ] **Step 5: Assert the baseline-hash bookkeeping**

Confirm that after a hot apply the watcher's `baselineHash` equals the new file content hash, so the next unrelated event does not re-trigger. Add the assertion to the Task 6.7 hot-apply test if not already covered.

- [ ] **Step 6: Run the full daemon package**

Run: `go test ./internal/daemon/ -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/daemon/config_watcher.go
git commit -m "Route config watcher through ClassifyConfigChange for hot-apply"
```

### Task 6.8: Live validation, hot apply and topology change

**Files:**
- Modify: `test/live/reload_live_test.go`

- [ ] **Step 1: Add the live hot-apply and topology scenarios**

```go
// TestLiveHotApplyNoReexec edits a config-only field (a model alias) and
// asserts the daemon pid is unchanged and the new alias resolves on the next
// /v1/models or chat request.
func TestLiveHotApplyNoReexec(t *testing.T) {
	// capture pid before edit; edit fake config alias; wait for
	// daemon.config.applied_in_process; assert pid unchanged; query adapter.
}

// TestLiveTopologyChangeReexecs edits a fake adapter port and asserts the
// change routes to quiet-wait then reload/rebind: pid changes and there is no
// bind gap on the unchanged listeners.
func TestLiveTopologyChangeReexecs(t *testing.T) {
	// capture pid; edit fake adapter port; assert pid changes, daemon serves
	// on the new port, and unchanged listeners stay bound throughout.
}
```

Read pid from the temp-state daemon pid file, and `daemon.config.applied_in_process` and `daemon.config_watch.*` from the temp-state logs. Always `h.assertProductionUntouched(t)`.

- [ ] **Step 2: Run the live apply scenarios**

Run: `go test -tags live ./test/live/ -run 'TestLive(HotApply|TopologyChange)' -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add test/live/reload_live_test.go
git commit -m "Add live hot-apply and topology-change reexec scenarios"
```

---

## Phase 7: Full validation and the optional Cursor scenario

### Task 7.1: Full gate run from the worktree

- [ ] **Step 1: Run every make gate**

Run: `make test lint fmt staticcheck staticcheck-extra deadcode audit govulncheck build`
Expected: all green.

- [ ] **Step 2: fmt leaves no diff**

Run: `make fmt && git diff --exit-code`
Expected: exit 0.

- [ ] **Step 3: Run the full live suite (preflight-gated)**

Run: `go test -tags live ./test/live/ -v`
Expected: all PASS; preflight refuses if any fake port is occupied.

- [ ] **Step 4: Commit any fixes**

```bash
git add -A && git commit -m "Resolve full-gate findings after lifecycle and hot-apply work" || true
```

### Task 7.2: Optional Cursor long-run Cloudflare scenario

**Files:**
- Modify: `test/live/reload_live_test.go` (guard behind an env flag, since it needs a real Cursor)

- [ ] **Step 1: Add the guarded Cursor scenario**

```go
// TestLiveCursorCloudflareDrain requires a real Cursor pointed at the fake
// app.cursor MITM port (58725). It opens a long-lived IDE backend connection,
// triggers a reload mid-connection, and asserts the in-flight request completes
// on the old generation while the idle tunnel is force-closed at idle-grace
// rather than pinning the cap. Skipped unless CLYDE_LIVE_CURSOR=1.
func TestLiveCursorCloudflareDrain(t *testing.T) {
	if os.Getenv("CLYDE_LIVE_CURSOR") != "1" {
		t.Skip("set CLYDE_LIVE_CURSOR=1 with a Cursor pointed at [::1]:58725")
	}
	// boot daemon on fake ports; point Cursor http.proxy at http://localhost:58725;
	// open a long-lived backend connection; trigger reload; read
	// logs/providers/mitm/wire.jsonl and the drain idle-force-closed event.
}
```

The assertion reads `logs/providers/mitm/wire.jsonl` under the temp state root for the in-flight request completing on the old generation, and the `livetrack.drain.idle_force_closed` event for the parked tunnel.

- [ ] **Step 2: Run it when a Cursor is available**

Run: `CLYDE_LIVE_CURSOR=1 go test -tags live ./test/live/ -run TestLiveCursorCloudflareDrain -v`
Expected: PASS, or SKIP when the flag is unset.

- [ ] **Step 3: Commit**

```bash
git add test/live/reload_live_test.go
git commit -m "Add optional live Cursor Cloudflare drain-on-reload scenario"
```

### Task 7.3: Documentation and teardown

**Files:**
- Create: `docs/reload-and-hot-apply.md`
- Modify: `CLAUDE.md`/`AGENTS.md` only if the durable rule changed

- [ ] **Step 1: Write the operator and agent doc**

`docs/reload-and-hot-apply.md`: describe the `livetrack.Group` lifecycle (phases, budgets, `Quiesce`, `AwaitQuiet`), the watcher decision (`ClassifyConfigChange` routing to hot-apply, reload, rebind, or restart-required), the hot-set versus reload-set fields, and the quiet-wait behavior. Point the `AGENTS.md` "Tracked Long-Lived Work" and "Daemon Reload" sections at it with one line each. Keep it a runbook in `docs/`, not in `AGENTS.md`.

- [ ] **Step 2: Update the AGENTS.md pointer if the durable rule changed**

The rule "subsystems MUST register with livetrack" is now enforced by the `Attach`-only construction choke. Add one sentence noting the enforcement moved from convention to API, pointing at the doc. Do not expand `AGENTS.md` further.

- [ ] **Step 3: Commit**

```bash
git add docs/reload-and-hot-apply.md CLAUDE.md
git commit -m "Document lifecycle group, quiet-wait, and config hot-apply"
```

- [ ] **Step 4: Teardown verification**

Confirm the live suite's teardown stopped every worktree daemon, removed temp dirs, and that the production daemon's socket and pid are unchanged. Run preflight once more and check the production socket:

Run: `go test -tags live ./test/live/ -run TestPreflight -v`
Expected: preflight passes (no fake port lingering), and the production daemon's pid file is the value it held before the run.

- [ ] **Step 5: Final commit**

```bash
git add -A && git commit -m "Finalize lifecycle group and config hot-apply branch" || true
```

---

## Self-Review

**Spec coverage:**
- Headline refactor (`Group`, `Attach` choke, `Quiesce`, one wait loop, deletions, phases and budgets, migration order, lint extension, capture-store fix): Phases 2 through 4 (Tasks 2.1 through 4.7). Covered.
- Feature 1 quiet-wait (watcher gate, idle-vs-active, max-wait bound, revert-during-wait skip): Tasks 5.1 through 5.2. Covered.
- Feature 2 hot-apply (`ClassifyConfigChange`, adapter snapshot, MITM apply, worker restart, watcher routing, conservative default, build-fail safety): Tasks 6.1 through 6.8. Covered.
- Acceptance criteria (isolation, temp roots, fake ports, preflight, unit and contract gates, live scenarios, Cursor, teardown): Task 0.2 harness plus Tasks 5.2, 6.8, 7.1 through 7.3. Covered.
- Out-of-scope items (no listener or store-path hot-apply, no CLI quiet-wait, no apply RPC): respected. The classifier routes them to reload or rebind, and the CLI path stays untouched.

**Placeholder scan:** Live-scenario test bodies and a few daemon helper bodies (`restartWorkersForConfig`, recording-runtime stubs) carry brief `/* ... */` markers where they bind to runtime handles that exist only in the codebase. Each is specified with its exact signature and assertions, not left vague. The largest extraction (Task 6.3) is described as a relocation with a continuous-test loop, because the moved fields are whatever `adapter.New` builds today.

**Type consistency:** `Route` values, `Phase`/`Budget`/`MemberSpec` fields, and the `Attach`/`Quiesce`/`AwaitQuiet`/`ActiveCount` signatures are used identically across tasks. `ApplyConfig`/`applyConfigInProcess`/`buildConfigSnapshot`/`snapshotHolder` names match between definition and call sites. `budgetReload`/`budgetShutdown`/`reloadQuietWait` are defined once (Task 3.1) and referenced consistently. The `New`/`newRegistry` and `DrainWith`/`drainWith` rename is flagged at every forward reference (Tasks 2.2, 2.3, 4.1) with the "use the current name until Task 4.1" note.
