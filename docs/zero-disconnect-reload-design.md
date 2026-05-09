# Zero-disconnect daemon reload design (CLYDE-300 to CLYDE-303)

Date: 2026-05-08. Base commit: `34a98d1`. Repo: `/Users/agoodkind/Sites/clyde-dev/clyde`.

## 1. Executive summary

- Phase 1 (CLYDE-300): live-runtime closers return nil when `reason == "daemon.reload"`; Codex app-server and Claude remote worker spawn with `SysProcAttr{Setsid: true}`; new worker reattaches via runtime-dir manifests on startup. Brief gRPC stream blip remains.
- Phase 2 (CLYDE-301): new `internal/clydesup` package promotes the supervisor to RPC owner of every live runtime subprocess; SIGCHLD reaped supervisor-side; worker becomes an RPC client; worker crash leaves no orphans.
- Phase 3 (CLYDE-302): supervisor owns lumberjack writers and the `capture.jsonl` flock; worker ships lines over the same socket with bounded inbox and a monotonic drop counter surfaced at WARN.
- Phase 4 (CLYDE-303): user-facing gRPC streams terminate at the supervisor; supervisor relays to the active worker; bounded per-stream ring buffer covers the swap window so consumers see no EOF, no gaps, no duplicates.
- Cross-cutting: every long-lived state holder on either side of the supervisor/worker boundary registers with `internal/livetrack`; all wire payloads are typed structs, no `any` and no empty marker frames.

## 2. Cross-cutting types and interfaces

Two new packages, both leaf packages. Names chosen for AGENTS.md "full domain names" rule:

- `internal/supproto`: wire types only, depends only on stdlib and `internal/correlation`. Imported by both supervisor and worker.
- `internal/clydesup`: supervisor-side server, registry, reaper, capture writer, gRPC relay. Lives outside `internal/daemon` because that package is worker-side.

Rejected `internal/sup/` (abbreviation) per AGENTS.md `## Documentation hygiene`.

### 2.1 `internal/supproto` shape

```go
type FrameType string // hello, hello_ack, spawn_runtime_request, spawn_runtime_response,
                       // attach_runtime_request, attach_runtime_response,
                       // list_runtimes_request, list_runtimes_response,
                       // stop_runtime_request, stop_runtime_response,
                       // runtime_event, write_capture_line, write_capture_ack,
                       // stream_subscribe, stream_subscribe_ack, stream_data,
                       // stream_cancel, error
type RuntimeKind string // codex.app_server, claude.remote_worker
type RuntimeID string
type RuntimeState string // running, exited
type StdinMode string  // null, pipe
type OutputMode string // null, tail, pipe
type StopSignal string // interrupt, term, kill
type ErrorCode string  // invalid_request, not_found, unavailable, internal
type RuntimeEventKind string // started, exited, stderr

type Frame struct {
    Type      FrameType    `json:"type"`
    RequestID string       `json:"request_id"`
    Payload   FramePayload `json:"payload"`
}
// FramePayload is a discriminated union with one field populated; all others nil.
type FramePayload struct {
    Hello                 *HelloPayload                 `json:"hello,omitempty"`
    HelloAck              *HelloAckPayload              `json:"hello_ack,omitempty"`
    SpawnRuntimeRequest   *SpawnRuntimeRequest          `json:"spawn_runtime_request,omitempty"`
    SpawnRuntimeResponse  *SpawnRuntimeResponse         `json:"spawn_runtime_response,omitempty"`
    AttachRuntimeRequest  *AttachRuntimeRequest         `json:"attach_runtime_request,omitempty"`
    AttachRuntimeResponse *AttachRuntimeResponse        `json:"attach_runtime_response,omitempty"`
    ListRuntimesRequest   *ListRuntimesRequest          `json:"list_runtimes_request,omitempty"`
    ListRuntimesResponse  *ListRuntimesResponse         `json:"list_runtimes_response,omitempty"`
    StopRuntimeRequest    *StopRuntimeRequest           `json:"stop_runtime_request,omitempty"`
    StopRuntimeResponse   *StopRuntimeResponse          `json:"stop_runtime_response,omitempty"`
    RuntimeEvent          *RuntimeEvent                 `json:"runtime_event,omitempty"`
    WriteCaptureLine      *WriteCaptureLineRequest      `json:"write_capture_line,omitempty"`
    WriteCaptureAck       *WriteCaptureLineAck          `json:"write_capture_ack,omitempty"`
    StreamSubscribe       *StreamSubscribeRequest       `json:"stream_subscribe,omitempty"`
    StreamSubscribeAck    *StreamSubscribeAck           `json:"stream_subscribe_ack,omitempty"`
    StreamData            *StreamDataPayload            `json:"stream_data,omitempty"`
    StreamCancel          *StreamCancelRequest          `json:"stream_cancel,omitempty"`
    Error                 *ErrorPayload                 `json:"error,omitempty"`
}

type HelloPayload struct{ ProtocolVersion uint32; WorkerPID int; WorkerGen uint64 }
type HelloAckPayload struct{ SupervisorPID int; SupervisorGen uint64 }
type RuntimeSessionMeta struct{ Provider, LiveSessionID, SessionName, Lease string }
type SpawnRuntimeRequest struct{
    Kind RuntimeKind; Executable string; Args, Env []string
    WorkDir, RuntimeDir string
    StdinMode StdinMode; StdoutMode, StderrMode OutputMode
    AttachThreadID string; SessionMeta RuntimeSessionMeta
}
type SpawnRuntimeResponse struct{ RuntimeID RuntimeID; PID int }
type AttachRuntimeRequest struct{ RuntimeID RuntimeID; AttachThreadID string }
type AttachRuntimeResponse struct{ RuntimeID RuntimeID; PID int; Kind RuntimeKind; StartedAtNanos int64 }
type ListRuntimesRequest struct{ KindFilter RuntimeKind }
type ListRuntimesResponse struct{ Runtimes []RuntimeInfo }
type RuntimeInfo struct{
    RuntimeID RuntimeID; Kind RuntimeKind; PID int; StartedAtNanos int64
    SessionMeta RuntimeSessionMeta; AttachThreadID string; State RuntimeState
}
type StopRuntimeRequest struct{ RuntimeID RuntimeID; Signal StopSignal; GraceMS uint32 }
type StopRuntimeResponse struct{ RuntimeID RuntimeID; ExitCode int; Signaled bool }
type RuntimeEvent struct{
    RuntimeID RuntimeID; Kind RuntimeEventKind
    StderrTail string; ExitCode int; Signal string
}
type CaptureFilePolicyWire struct{
    RotationEnabled bool; MaxSizeMB, MaxBackups, MaxAgeDays int; Compress bool
}
type WriteCaptureLineRequest struct{ Dir string; Line []byte; Policy CaptureFilePolicyWire }
type WriteCaptureLineAck struct{ Accepted bool; Dropped uint64; Reason string }
type StreamSubscribeRequest struct{
    Method, StreamID, SubscriberID string
    InitialReq []byte; ReplaySinceSeq uint64
}
type StreamSubscribeAck struct{ StreamID string; HighestSeq uint64 }
type StreamDataPayload struct{ StreamID string; Seq uint64; Bytes []byte; EOF bool }
type StreamCancelRequest struct{ StreamID, Reason string }
type ErrorPayload struct{ Code ErrorCode; Message string }
```

### 2.2 `internal/clydesup` shape

```go
type Runtime struct{
    ID supproto.RuntimeID; Kind supproto.RuntimeKind; PID int
    Cmd *exec.Cmd; StartedAt time.Time
    SessionMeta supproto.RuntimeSessionMeta; AttachThreadID string
    State supproto.RuntimeState; ExitCode int
    StderrTail *boundedTail; runtimeDir string
}
type SupervisorMeta struct{ Kind supproto.RuntimeKind; LiveSessionID string; PID int }
func (SupervisorMeta) IsLivetrackMeta() {}
type CaptureMeta struct{ Path string }
func (CaptureMeta) IsLivetrackMeta() {}
type StreamMeta struct{ Method, StreamID, SubscriberAddr string }
func (StreamMeta) IsLivetrackMeta() {}

type SupervisorRuntimeRegistry struct{
    log *slog.Logger
    track *livetrack.Registry[SupervisorMeta]
    mu sync.RWMutex
    byID map[supproto.RuntimeID]*Runtime
    byThread map[string]supproto.RuntimeID
}
type Reaper struct{ log *slog.Logger; sigCh chan os.Signal
    runtimes *SupervisorRuntimeRegistry; eventBus *eventBus }
```

### 2.3 Worker-side client (`internal/daemon`)

```go
type SupervisorClient interface{
    Hello(ctx context.Context) error
    SpawnRuntime(ctx context.Context, req supproto.SpawnRuntimeRequest) (supproto.SpawnRuntimeResponse, error)
    AttachRuntime(ctx context.Context, req supproto.AttachRuntimeRequest) (supproto.AttachRuntimeResponse, error)
    ListRuntimes(ctx context.Context, kind supproto.RuntimeKind) ([]supproto.RuntimeInfo, error)
    StopRuntime(ctx context.Context, req supproto.StopRuntimeRequest) (supproto.StopRuntimeResponse, error)
    SubscribeRuntimeEvents(ctx context.Context) (<-chan supproto.RuntimeEvent, error)
    WriteCaptureLine(ctx context.Context, req supproto.WriteCaptureLineRequest) error
    Close() error
}
```

For Phase 3, MITM gets a typed sink so it does not import daemon symbols:

```go
// internal/mitm/capture_sink.go
type CaptureSink interface{
    WriteLine(ctx context.Context, dir string, line []byte, policy CaptureFilePolicy) error
    Close() error
}
```

## 3. Unix socket protocol

Length-prefixed JSON, not protobuf. Justification: zero new deps, payloads small and infrequent, the existing supervisor reload control on `daemon.supervisor.sock` already speaks JSON (`internal/daemon/supervised_reload.go:26`, `internal/daemon/supervisor.go:373`). Phase 4's `StreamData.Bytes` carries already-encoded protobuf bytes so an outer protobuf wrap adds no benefit.

Wire format per frame: `[uint32 BE length N][N bytes JSON-encoded supproto.Frame]`. Maximum frame size 4 MiB; oversize frames are rejected with `ErrorCodeInvalidRequest`.

Socket path: existing `supervisorSocketPath()` at `internal/daemon/supervisor.go:179`, equal to `filepath.Join(config.RuntimeDir(), "daemon.supervisor.sock")`. Multiplexed: a JSON frame missing the `"type"` field falls through to the legacy reload handler at `supervisor.go:309`, so old-supervisor + new-worker rollouts still work. Per AGENTS.md `## Networking and security`: Unix socket only.

Operations:
- `SpawnRuntime(req) -> resp`: supervisor exec.Cmd.Start with `SysProcAttr{Setsid: true}`, registers in `SupervisorRuntimeRegistry`, returns `RuntimeID`+PID.
- `AttachRuntime(req) -> resp`: lookup by `RuntimeID` or `AttachThreadID`; `not_found` if neither matches.
- `ListRuntimes(req) -> resp`: snapshot of the supervisor registry filtered by `KindFilter`. New workers call this on startup.
- `StopRuntime(req) -> resp`: supervisor sends `Signal`, waits up to `GraceMS`, escalates to SIGKILL.
- `WriteCaptureLine(req) -> ack`: supervisor enqueues to per-path inbox; ack carries monotonic `Dropped` count.
- `SubscribeRuntimeEvents(req)`: long-lived push channel; supervisor sends `RuntimeEvent` frames until worker disconnect or `StreamCancel`.

Error envelope: any malformed request returns a `Frame{Type: error, RequestID: original, Payload: FramePayload{Error: &ErrorPayload{Code, Message}}}`.

## 4. File and package layout

### 4.1 Phase 1 (no new package)

- Modified: `internal/daemon/livetrack_closer_live.go` (closers gain reload-aware no-op).
- Modified: `internal/providers/codex/lifecycle/appserver.go:395-438` (Setsid, drop CommandContext).
- Modified: `internal/daemon/server.go:2584-2589` (Setsid on Claude remote worker).
- Added: `internal/providers/codex/lifecycle/runtime_dir.go` (~60 LOC) for manifest read/write.
- Added: `internal/daemon/runtime_reattach.go` (~80 LOC) for startup scan-and-attach.

No removes or renames.

### 4.2 Phase 2

- Added: `internal/supproto/{frames.go,codec.go,codec_test.go}` (length-prefix JSON read/write).
- Added: `internal/clydesup/{server.go,registry.go,runtime.go,spawn.go,reaper.go}`.
- Added: `internal/daemon/supervisor_client.go` and `internal/daemon/supervisor_client_codex.go`.
- Modified: `internal/daemon/supervisor.go:60-151` (multiplexed dispatcher; spawn `clydesup.Server` and `Reaper` goroutines).
- Modified: `internal/providers/codex/lifecycle/appserver.go startProcess` and `internal/daemon/server.go startRemoteWorkerProcess`: prefer `SupervisorClient.SpawnRuntime` when `envDaemonSupervisorSocket` is set; fall back to in-process spawn otherwise (keeps non-darwin tests working).

### 4.3 Phase 3

- Added: `internal/clydesup/capture_writer.go`.
- Added: `internal/mitm/capture_sink.go` (interface + `LocalCaptureSink`).
- Added: `internal/daemon/supervisor_capture_sink.go` (`SupervisorCaptureSink`).
- Modified: `internal/mitm/proxy.go:708` (`WriteCaptureEvent`) and `internal/mitm/raw_capture.go:255` (`appendCursorCaptureEventAtDir`): both call `proxy.captureSink.WriteLine` instead of package-level `WriteCaptureLine`.
- Modified: `internal/mitm/capture_policy.go`: keep functions for the local-sink path; no behavior change.

### 4.4 Phase 4

- Added: `internal/clydesup/grpc_relay.go` (gRPC server registering streaming methods only).
- Added: `internal/clydesup/stream_swap.go` (per-stream ring buffer, replay protocol, dedup by `Seq`).
- Modified: `internal/daemon/run.go:325-405` (worker gRPC server moves to `config.RuntimeDir()/daemon.worker.<gen>.sock`).
- Modified: `internal/daemon/run.go:604-655` (`reloadDaemonBinary`): remove `drainReloadedLiveWorkers` call at line 646; supervisor relay handles cutover.

## 5. Phase 1 details (CLYDE-300)

Read first: `internal/daemon/run.go:1393-1426`, `internal/daemon/livetrack_closer_live.go`, `internal/providers/codex/lifecycle/appserver.go:395-438`, `internal/daemon/server.go:2550-2605`.

### 5.1 Closer no-op on reload reason

In `internal/daemon/livetrack_closer_live.go`:

- `codexRuntimeCloser.Close(reason string) error` (line 19): if `strings.TrimSpace(reason) == "daemon.reload"`, return nil and do not call `c.runtime.Close()`. The runtime is detached via Setsid; the new worker reattaches.
- `claudeWorkerCloser.Close(reason string) error` (line 43): same condition; do not signal the subprocess. Inject-socket presence drives reattach.

The `"daemon.reload"` reason already flows from `drainReloadedLiveWorkers` at `run.go:1411` through `livetrack/closer.go:45` to the closer. No call-site change.

### 5.2 Setsid on subprocess spawn sites

- `internal/providers/codex/lifecycle/appserver.go:397`: change `exec.CommandContext(ctx, name, args...)` to `exec.Command(name, args...)` (the ctx-bound form would be canceled when the worker context dies on reload, defeating detachment). Add `cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}` immediately before `cmd.Start()` at line 415. Add `import "syscall"`.
- `internal/daemon/server.go:2584`: add `cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}` immediately before `cmd.Start()` at line 2589. The package already imports `syscall`.

Build: darwin and linux both support Setsid; no new build tags. Setsid puts the child in a new session and process group; on macOS this stays inside the launchd job (still SIGTERM'd on user logout, which is what we want).

### 5.3 Runtime-dir manifest and reattach

`internal/providers/codex/lifecycle/runtime_dir.go` shape:

```go
type CodexRuntimeManifest struct{
    ThreadID string; PID int; StartedAtNanos int64
    WorkDir, Model string; SocketPath string
}
func CodexRuntimeDir(threadID string) string {
    return filepath.Join(config.RuntimeDir(), "codex", threadID)
}
func WriteCodexRuntimeManifest(threadID string, m CodexRuntimeManifest) error
func ReadCodexRuntimeManifests() ([]CodexRuntimeManifest, error)
func RemoveCodexRuntimeManifest(threadID string) error
```

Write hook: in `AppServerRuntime.Start` after thread/start succeeds (line 119) and in `Attach` (line 153). Remove hook: only on natural exit (after `cmd.Wait` in `Close`); never on the reload-reason no-op path. The existing `srv.preserveRuntimeDirsOnClose()` call at `run.go:644` already prevents runtime-dir wipe on reload.

`internal/daemon/runtime_reattach.go` walks `ReadCodexRuntimeManifests`, skips entries where `syscall.Kill(pid, 0)` returns ENOENT, calls `runtime.Attach`, populates `s.liveSessions`, and registers via `s.registerCodexLiveSession`. Hook from `daemon.New` after the bridge watcher start (`server.go:323`).

### 5.4 Tests

- `internal/providers/codex/lifecycle/setsid_test.go`: spawn a `sleep 30` via the runtime, exit the parent, assert child alive after 1 s. Skip on Windows.
- `internal/daemon/runtime_reattach_test.go`: write a fake manifest pointing at a `sleep 30` child, call the startup hook, assert `s.liveSessions` and `s.liveWorkers.Count() == 1`.
- `internal/daemon/livetrack_closer_live_test.go`: new cases for `Close("daemon.reload")` returning nil; `Close("force_close.deadline")` still calls runtime.Close.
- Extend `internal/daemon/supervised_reload_test.go`: register a fake codex runtime, call `reloadDaemonBinary`, assert subprocess PID still alive after the reload returns.

## 6. Phase 2 details (CLYDE-301)

The macOS supervisor today is ~200 LOC at `internal/daemon/supervisor.go`. Phase 2 grows it into a small RPC daemon. The `clydesup` package owns the new logic; `daemon` keeps only the worker-side client.

`SupervisorRuntimeRegistry` adopts livetrack. Each `Runtime` is registered on spawn with a closer that, by reason: `supervisor.shutdown` -> SIGTERM with grace then SIGKILL; `worker.disconnect` -> no-op (subprocess crosses worker generations); other -> SIGKILL after 1 s. The registry is the source for `ListRuntimes`.

The reaper goroutine subscribes to SIGCHLD, runs `syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)` in a loop until pid<=0, looks up by PID, marks the runtime exited, publishes a `RuntimeEventExited` frame. Started in `Supervise` after the legacy reload listener; lifecycle bound to the supervisor process.

Worker-side client (`internal/daemon/supervisor_client.go`) is created on worker startup when `os.Getenv(envDaemonSupervisorSocket) != ""`. Behavior:
- Hellos on connect; rejects on `HelloAck.SupervisorGen` mismatch (the worker should exit so launchd restarts a fresh pair).
- Holds one open `SubscribeRuntimeEvents`; on `RuntimeEventExited` for a tracked runtime the worker releases the livetrack session and removes it from `s.liveSessions`.
- All RPCs use a 5 s default deadline.

Failure modes:
- Supervisor unreachable on `SpawnRuntime`: return `codes.Unavailable` to the gRPC caller. Do not fall back to in-process spawn; that defeats CLYDE-301.
- Supervisor unreachable on `WriteCaptureLine`: drop and increment a local counter; WARN every 100 drops.
- Worker crash: supervisor's reaper sees the worker exit (already handled at `supervisor.go:171`); supervisor restarts the worker; runtimes stay in the supervisor registry; the new worker's startup `ListRuntimes` rediscovers them.
- Supervisor crash: launchd restarts the supervisor; macOS lacks `PR_SET_CHILD_SUBREAPER`, so subprocesses reparented to launchd are orphans until next reboot. This is the rare path; we accept it because the change reduces *worker* crash blast radius from 100% to 0%.

## 7. Phase 3 details (CLYDE-302)

`internal/clydesup/capture_writer.go` owns a `CaptureWriterRegistry` that holds the lumberjack writer cache (currently `internal/mitm/capture_policy.go:36-42`) and the flock supervisor-side. Configured at supervisor startup from `[adapter.wire_capture.rotation]` via `config.LoadGlobalOrDefault`. The user-visible config key is unchanged; no migration note required.

Each `captureOwner` runs a goroutine reading from a bounded inbox (4096 slots) and writing through lumberjack. On a 0.5 s drain budget overrun, emit one `slog.Warn("clydesup.capture.drain_slow", "component", "supervisor", "concern", "providers.mitm")`. Rotation runs inside lumberjack on the supervisor; the `path + ".lock"` file is unaffected by content rotation, so the flock survives rotation.

Worker-side: a fixed 8192-slot buffered channel feeds `SupervisorClient.WriteCaptureLine`. On full, drop and increment `local_dropped`. Every 100 drops emit `slog.Warn("mitm.capture.dropped_local", "component", "mitm", "concern", "providers.mitm", "dropped_local", local_dropped, "dropped_supervisor", lastAck.Dropped)`. The supervisor's monotonic `Dropped` count in the ack lets the worker detect divergence.

Wiring: `internal/mitm/proxy.go` constructor takes a `CaptureSink`. The default is `LocalCaptureSink` (calls `WriteCaptureLine`). The daemon wires `SupervisorCaptureSink` when the supervisor socket is set, falling back to local otherwise.

Tests:
- `internal/clydesup/capture_writer_test.go` stress: 10 goroutines x 100 KB lines x 2 s; assert `Dropped > 0`, JSONL well-formed, `MaxBackups` honored.
- `internal/daemon/supervisor_capture_sink_test.go`: bidi end-to-end with a fake supervisor; asserts `Dropped` propagation and shutdown drains the inbox.

## 8. Phase 4 details (CLYDE-303)

The supervisor binds the inherited daemon listener (`listenerNameDaemon`). Its gRPC server registers `clydev1.UnimplementedClydeServiceServer` and overrides only the streaming methods (`SubscribeRegistry`, `SubscribeProviderStats`, `StreamLiveSession`, `TailTranscript`, `CompactPreview`, `CompactApply`); unary methods are forwarded to the active worker via a thin gRPC proxy connection on `daemon.worker.<gen>.sock`.

Phased delivery (only 4a is in scope for CLYDE-303):
- 4a: `StreamLiveSession` only (highest user value, smallest API surface).
- 4b: `TailTranscript` (out of scope).
- 4c: remaining streaming methods (out of scope).

Stream relay handler:
1. Pick `activeWorker`.
2. Send `StreamSubscribeRequest{Method, StreamID, InitialReq, ReplaySinceSeq=0}`.
3. Receive `StreamSubscribeAck{HighestSeq}`.
4. Read `StreamDataPayload{Seq, Bytes}` frames; write `Bytes` directly to the gRPC stream.
5. On worker swap mid-stream: cancel the old subscription with `StreamCancel`, send a fresh `StreamSubscribe` to the new worker with `ReplaySinceSeq = lastDeliveredSeq + 1`.

Replay buffer: per-stream ring on the worker, capacity 1024 events, populated on every send. `Seq` is strictly monotonic per `StreamID`. If the new worker cannot replay the requested `Seq` (it never started this stream, or it overflowed the ring), it returns `ErrorCodeNotFound`; the supervisor returns `codes.Unavailable` and the gRPC client retries.

Reload swap:
1. New worker hellos.
2. Supervisor sets `activeWorker` to new; new `StreamSubscribe` calls go there.
3. In-flight streams stay on `previousWorker` until the swap-window timer (default 30 s) elapses or the stream completes naturally.
4. Timer fires: supervisor sends `StreamCancel` on previous, resubscribes on active with the recorded seq.
5. `previousWorker` exits normally.

The `StreamMeta` livetrack registry is the inventory the swap timer iterates.

Backpressure:
- Per-stream, a 64-event channel between the worker reader and the gRPC writer. If gRPC writer falls behind, channel fills, worker reader blocks on send, worker pauses sending more `StreamData` for that `StreamID`. Slow-consumer DoS bound: per-stream timeout (default 30 s) drops the slow consumer with `codes.DeadlineExceeded`.
- Worker-side: if production exceeds supervisor read rate, the ring buffer drops oldest events and emits `slog.Warn("clydesup.stream.replay_loss", ...)`. Next reconnect with stale seq returns `not_found`; consumer reconnects.

Tests:
- `internal/clydesup/stream_swap_test.go`: emit 100 events, swap workers at event 50, assert consumer sees 1..100 in order with no gaps and no duplicates.
- `internal/daemon/reload_during_active_stream_test.go`: integration test with a real daemon binary, opens `StreamLiveSession`, triggers reload, asserts no `EOF`, no missing events, no duplicates.

## 9. Test strategy per phase

| Phase | Unit | Integration |
|-------|------|-------------|
| 1 | closer reason no-op; manifest read/write | Codex subprocess survives worker exit; reattach populates `s.liveSessions` |
| 2 | reaper on fake `Wait4`; client hello rejection on gen mismatch | Worker crash leaves runtime alive; new worker `ListRuntimes` discovers it |
| 3 | bounded inbox drop accounting; rotation across the boundary | 10 producers x 100 KB x 2 s stress; flock survives reload |
| 4 | ring-buffer replay on `ReplaySinceSeq` | Reload during `StreamLiveSession`: no `EOF`, no gaps, no duplicates |

`make test` runs both. SIGCHLD-reaping integration test is darwin+linux (`_unix.go` build tag); Windows skipped.

## 10. AGENTS.md additions

Phase 1: under `### Daemon reload` after the existing "Active session runtime dirs must survive reload drain" bullet, insert:

```
- Live-runtime closers (Codex app-server, Claude remote worker) MUST observe
  the reload reason "daemon.reload" and return without signaling the
  subprocess. The subprocess is detached via Setsid at spawn time and the
  new worker reattaches by scanning runtime-dir manifests on startup.
```

Phase 2: new section after `### Daemon-owned live sessions`:

```
### Daemon-owned subprocess ownership

The supervisor process owns every long-lived subprocess (Codex app-server
runtimes, Claude remote workers). Workers spawn, attach, and stop runtimes
through the supervisor RPC defined in internal/supproto. SIGCHLD reaping is
supervisor-only. Worker code MUST NOT call exec.Cmd.Start for live-runtime
subprocesses; use SupervisorClient.SpawnRuntime instead. The supervisor's
runtime registry adopts internal/livetrack with SupervisorMeta.
```

Phase 3: replace the existing wire-capture rotation bullet under `## Logging and observability`:

```
- Wire capture is per-provider, configured under
  `[adapter.<provider>.wire_capture]`. A shared rotation budget at
  `[adapter.wire_capture.rotation]` keeps on-disk volume bounded. The
  supervisor process owns the lumberjack writers and the capture.jsonl
  flock; workers ship lines through SupervisorClient.WriteCaptureLine,
  which is bounded, and dropped lines are surfaced as slog WARN events
  with monotonic counters.
```

Phase 4: append to `### Daemon reload`:

```
- User-facing gRPC streams terminate at the supervisor and are multiplexed
  to the active worker. Reload swaps the active worker and resubscribes
  in-flight streams against the new worker using a bounded replay buffer;
  consumers see no EOF, no missing events, and no duplicates within the
  swap window.
```

## 11. Migration order and dependencies

- Phase 1 is independent. It can land first against `main`. Files: `livetrack_closer_live.go`, `appserver.go`, `server.go` (the spawn site only); `run.go` is not required.
- Phase 2 depends on Phase 1's Setsid spawn and runtime-dir manifest. Phase 2 reuses the manifest schema. The new `clydesup` and `supproto` packages have no merge conflict with Phase 1.
- Phase 3 depends on Phase 2 (reuses the supervisor RPC socket). Modifies `internal/mitm/proxy.go` and `internal/mitm/raw_capture.go`; isolated from Phase 1 and Phase 2 file lists.
- Phase 4 depends on Phase 2. Heavy edit to `internal/daemon/run.go`. Land after Phases 1, 2, 3 are merged.

Parallel start across four worktrees:
- Subagent 1 (Phase 1): start now.
- Subagent 2 (Phase 2): start now in parallel; rebase on Phase 1 before merge. Shared file: `appserver.go` (Phase 1 sets Setsid; Phase 2 swaps spawn for SupervisorClient).
- Subagent 3 (Phase 3): start now in parallel; rebase on Phase 2. No overlap with Phase 1 file list.
- Subagent 4 (Phase 4): start `supproto` stream types and `clydesup/grpc_relay.go` now; the `internal/daemon/run.go` rewrite must wait until Phases 1, 2, 3 merge.

`run.go` is touched only by Phase 4; Phases 1, 2, 3 do not modify `run.go`.

## 12. Risks

Phase 1 (macOS): Setsid creates a new session/process group; under launchd it stays in the same launchd job and is still SIGTERM'd on user logout (acceptable). Risk: a future Go version returning an error on Setsid would break spawn; mitigation is a darwin-only build-tag fallback to `Setpgid`.

Phase 2 (macOS): SIGCHLD coalescing is well-documented; the `WNOHANG` reaper loop handles it. Risk: if Phase 4's gRPC server is slow to bind, workers' `Hello` could time out; mitigation is to run `Hello` on a goroutine separate from the gRPC bind. macOS lacks `PR_SET_CHILD_SUBREAPER`, so on supervisor crash, runtime subprocesses reparented to launchd are orphans until reboot; we accept this rare path.

Phase 3: rotation correctness. Risk: lumberjack's per-Logger mutex means two `Logger`s for the same path with different rotation parameters would fight for the file. Mitigation: the existing `captureWriterKey` includes rotation parameters; phase 3 rejects a second config for the same path with a slog WARN.

Phase 4: slow-consumer DoS. Risk: a wedged TUI on a slow link fills the per-stream channel and blocks the worker reader. Mitigation: per-stream 30 s timeout drops the slow consumer with `codes.DeadlineExceeded`. Second risk: supervisor crash takes the entire user-visible API down; mitigation is launchd KeepAlive and the worker keeps Codex sessions alive across the restart.

## 13. Out of scope

Phase 1: no supervisor RPC, no capture-writer ownership change, no stream relay. Stream blip on reload remains.

Phase 2: no user-facing gRPC stream relay. Capture writers stay worker-side. Stream blip remains.

Phase 3: no reorg of `[adapter.<provider>.wire_capture]`; only rotation is read by the supervisor. Worker-local fallback (supervisor unreachable) is the same as today.

Phase 4: only streaming methods, only `StreamLiveSession` (4a). Unary methods stay forwarded. Phases 4b (transcripts) and 4c (compact, registry) are tracked separately.

## 14. Build-on, do-not-rebuild

Phase 1 builds on existing CLYDE-275 foundations:

- `internal/daemon/run.go:644` already calls `srv.preserveRuntimeDirsOnClose()` before drain; the manifest survives reload because of this. Phase 1 must NOT remove it.
- `internal/daemon/server.go:256-258` already gates runtime-dir cleanup on a flag.
- `internal/livetrack/closer.go:43-58` already passes `reason` through to `Closer.Close`. Phase 1's check needs zero changes to livetrack.
- `internal/daemon/run.go:800` already sets `Setsid: true` on the direct (non-darwin) replacement-daemon spawn. Phase 1 extends this pattern to runtime subprocesses; `import "syscall"` is already in place.
- `internal/daemon/supervisor.go:60-151` is the existing supervisor entrypoint; Phase 2 extends it. The `controlListener` already exists.
- `internal/mitm/capture_policy.go:158-183` `releaseCaptureWriters` is the proven idempotent flock-release pattern Phase 3 makes the supervisor own.
- `internal/daemon/livetrack_meta.go` and `livetrack_meta_live.go` already adopt livetrack with typed Meta and `IsLivetrackMeta()` markers; `SupervisorMeta`, `CaptureMeta`, and `StreamMeta` follow the same pattern verbatim.
