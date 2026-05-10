# (Clyde CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) Revert: Handoff Document

**For the LLM picking up this conversation.** This document is your entire context. You have no memory of prior sessions. Read it top to bottom. Every concept is defined here. If something says "see X", X is defined elsewhere in this same doc.

---

## 1. What is Clyde?

Clyde is a Go-based command-line tool and background daemon written by Alex Goodkind. It lives at `/Users/agoodkind/Sites/clyde-dev/clyde`. Its purpose is to wrap and instrument LLM-using CLI tools (Claude Code CLI, Codex CLI, Cursor IDE) so the user can:

1. **Capture all their HTTPS traffic** to LLM providers (Anthropic, OpenAI, Cursor's API) for debugging, auditing, and replay. This is done via a man-in-the-middle proxy (MITM) that intercepts HTTPS by injecting its own CA certificate. The CLI tools are configured (via env var like `HTTPS_PROXY=[::1]:48723`) to route their network calls through this proxy.
2. **Manage interactive LLM sessions**. When the user runs `clyde claude`, Clyde acquires a per-session config (model selection, reasoning effort), sets up env vars to point at MITM, and execs the underlying `claude` binary. Same for `clyde codex` (execs the `codex` binary). Same for `clyde cursor` (launches a patched `Cursor.app` bundle).
3. **Expose its own OpenAI-compatible API** at `[::1]:11434` so Cursor (which speaks the OpenAI protocol for "Bring Your Own Key" model integrations) can call into Clyde to use Anthropic models via a Cursor-native UI.

The Clyde codebase is a Go monolith. One binary at `cmd/clyde/main.go` produces the `clyde` executable. Subcommands include `claude`, `codex`, `cursor`, `daemon`, `dashboard`, `mcp`, `mitm`, `hook`. The same binary, run with different argv, plays different roles.

---

## 2. Architecture overview

There are several independent subsystems. Confusing them is the root of every mistake in this session. **Pay attention to the layer separation.**

### 2.1 The daemon

A long-lived background process spawned by macOS `launchd` (the system service manager that starts processes at login). The daemon is `clyde daemon` and `clyde daemon worker`. The daemon does:

- Holds session state (which Claude/Codex/Cursor sessions exist, their metadata, which is currently active).
- Runs a gRPC server on `[::1]:<daemon-port>` and a Unix socket at `/var/folders/jq/.../clyde-501/daemon.sock`. Other parts of Clyde (TUI, CLI wrappers, hook scripts) connect to this socket to talk to the daemon.
- Hosts the MITM proxy (a separate TCP listener that intercepts HTTPS).
- Hosts the OpenAI-compatible adapter on `[::1]:11434`.
- Hosts the webapp (an HTTP UI) on a separate port.

There is a "daemon reload" operation. The user runs `clyde daemon reload` (or `make deploy` triggers it). The running daemon swaps its binary in place without dropping its listeners. The intent is zero downtime.

### 2.2 The supervisor + worker model (on darwin only)

On macOS, the `daemon` process is split into two:

- **Supervisor**: long-lived parent. Started by launchd. Lives across daemon reloads.
- **Worker**: short-lived child. Spawned by the supervisor. Replaced on each reload.

The supervisor's job is to spawn the worker, monitor it, and orchestrate reload (kill old worker, spawn new worker, hand it inherited file descriptors so listeners stay bound).

On non-darwin platforms (Linux), there is no supervisor. `clyde daemon` is just one process that does everything.  (**CORRECTION**: THIS IS WRONG!!!! ItS SUSPPOSED to be Systemd but LLM hallucinated)

The supervisor lives in `internal/daemon/supervisor.go`. As of `cb9fc7d` (current state of `main` branch), this supervisor is small (447 lines). It does NOT bind any public listeners. The worker binds them all. The supervisor only handles process lifecycle (spawn, readiness pipe, reload control via Unix socket).

### 2.3 The MITM proxy

An HTTPS-intercepting TCP listener. Bound at `[::1]:48723` by default (configurable). When a client (Claude CLI, Codex CLI, Cursor patched app) sends HTTPS traffic via `HTTPS_PROXY=[::1]:48723`, the MITM:

1. Receives the `CONNECT host:port` request.
2. Splits the TLS connection. It presents its own certificate (signed by Clyde's local CA which the client trusts) to the client. It opens a real TLS connection to the real upstream.
3. Decrypts traffic in the middle, captures it to JSONL files in `~/.local/state/clyde/mitm/...`, forwards it onward.

The MITM is **consumer-agnostic**. It serves whatever client is configured to point at it: Claude CLI, Codex CLI, Cursor patched app, Codex app. The proxy doesn't know or care which client is talking to it.

**Critical property**: the MITM TCP listener must have **zero bind-gap** across daemon reload. If the listener port is unbound for even a split second:

- Claude CLI's in-flight HTTP request fails.
- Claude CLI retries (showing "reconnecting 1/N" messages).
- The retry hits upstream session-token invalidation (Cloudflare/Anthropic/Cursor).
- The CLI gives up permanently.

This was the user's biggest pain. A single short bind-gap during reload would kill all their active LLM sessions, requiring restart.

The MITM lives in `internal/mitm/` (proxy.go, capture_policy.go, raw_capture.go, ws_capture.go, connect_tunnel.go).

### 2.4 The OpenAI-compatible adapter

A separate HTTP/gRPC (**CORRECTION**: Its HTTP only, gRPC is strictly between clyde concerns) server on `[::1]:11434`. Cursor IDE uses this for its "Bring Your Own Key" (BYOK) feature. Cursor lets you configure an OpenAI-compatible endpoint for model calls. The user has Clyde set up as that endpoint. Clyde translates Cursor's OpenAI-shaped requests into Anthropic-shaped requests (or whatever provider).

This is **completely separate** from the MITM proxy. The MITM intercepts Cursor's traffic to Cursor's own backend (`api2.cursor.sh`). The adapter handles Cursor's BYOK model traffic.

The adapter lives in `internal/adapter/`.

### 2.5 The TUI (terminal UI)

A `tcell`-based dashboard. Run as `clyde dashboard` or as the default `clyde` invocation (with no args). Shows the user a list of their sessions. Lets them resume, delete, rename, create new sessions.

The TUI is itself a process (the `clyde` binary running `dashboard` mode). It connects to the daemon over the Unix socket via gRPC to fetch the session list and subscribe to updates.

The TUI's key callbacks (wired in `cmd/root.go` around line 175-176):

- `StartSessionWithBasedir`. User wants to start a new Claude session.
- `StartLiveSession`. User wants to start a daemon-managed background session (the "live session" feature).
- `ResumeSession`. User wants to resume an existing session.
- Various delete/rename/show callbacks.

The TUI lives in `internal/ui/` (app.go is ~5000 lines).

### 2.6 The CLI wrappers

When the user runs `clyde claude` or `clyde codex`, this invokes a wrapper subcommand. The wrapper:

1. Acquires a session from the daemon via gRPC (`AcquireSession` RPC).
2. Sets env vars (`HTTPS_PROXY`, `NODE_EXTRA_CA_CERTS`, `ANTHROPIC_BASE_URL`, etc.) to route the upstream CLI through MITM.
3. Execs the real underlying CLI tool (e.g., `claude` from `$PATH`).

The Claude wrapper has a daemon-monitor goroutine (`internal/providers/claude/lifecycle/daemon_session.go:54-109`) that polls the daemon every 30 seconds and tolerates outages. The Codex wrapper does NOT have this monitor. It is a parity gap (CLYDE-310 (Codex wrapper has no daemon-monitor goroutine; parity with Claude wrapper which polls daemon every 30s) in Tack).

The wrappers live in `internal/providers/claude/lifecycle/` and `internal/providers/codex/lifecycle/`. The Cursor wrapper is special. It is a separate Swift app at `/Users/agoodkind/Sites/cursor-via-clyde-wrapper/` that sets the same env vars before launching `Cursor.app`.

### 2.7 Live sessions (a broken/unused feature)

The dashboard has a `StartLiveSession` callback. It's supposed to launch a daemon-managed background subprocess (a Codex app-server or a Claude remote worker), allowing the user to interact with it via stream/send RPCs from the TUI or webapp.

**The user confirmed 2026-05-09**: this feature does not work today and they have never used it.

The "live runtime" subprocess is what CLYDE-300 (live-runtime survival on reload via Setsid spawn + detach-only closers + filesystem manifest reattach for daemon-managed background subprocesses) (Setsid + manifest reattach) was protecting. Since the feature doesn't work, that protection is moot.

Live sessions live in `internal/daemon/live_sessions.go` (~600 lines), `internal/daemon/server.go` RPCs (`StartLiveSession`, `StreamLiveSession`, `SendLiveSession`), `internal/ui/app.go` callback at line ~5138, `internal/webapp/server.go` `handleStartLiveSession` HTTP endpoint.

### 2.8 The webapp

An HTTP server hosted by the daemon. Provides a browser UI for the dashboard. Lives in `internal/webapp/`.

### 2.9 The hook system

Claude Code CLI supports "hooks". Hooks are scripts that run on specific events (session start, prompt submit, etc.). Clyde uses this to keep its session state in sync. The hook handler is `clyde hook sessionstart` and friends.

### 2.10 The MCP server

`clyde mcp` runs an MCP (Model Context Protocol) server over stdio. Used by other tools that integrate with Clyde. Lives in `internal/mcpserver/`.

---

## 3. The user's actual stated requirements

Read this section carefully. Every misstep in this session came from misunderstanding what the user wanted.

The user's complaints in their own words:

> "MITM crashing out was the biggest blocker, and its not gRPC so its simple, but when MITM died even for split second it caused every code and claude chat to fail permanently until restart 'reconnecting 1/10....'"

> "The entire wrapper around the cli would fully crash when daemon restart happened."

> "TUI was crashing claude and codex sessions because it was buggy and wasn't resilient to daemon offline modes."

> "TUI was showing an epileptic flashing 'daemon offline'."

> "TUI is what execs the binary IIRC."

Decoded into independent concerns:

**Concern A**: MITM listener must have zero bind-gap on reload. When MITM was unbound for even a split second, every Claude/Codex/Cursor request mid-flight failed. The CLI tools' own retry logic ("reconnecting 1/N") then hit upstream session-token invalidation and gave up permanently. **The bind-gap is the trigger for a permanent-failure cascade.** This is the biggest blocker.

**Concern B**: The CLI wrapper code (`clyde claude`, `clyde codex`) needs to be resilient to brief daemon disconnects. Running `clyde claude` while the daemon is reloading should not kill the user's active CLI session. Specifically, the Claude wrapper has a 30s daemon-monitor goroutine. The Codex wrapper has nothing equivalent.

**Concern C**: The TUI needs to:

- Debounce its "DAEMON OFFLINE" badge so brief daemon outages don't cause flashing.
- Not crash itself or kill its child CLI sessions when the daemon is briefly offline.

The user explicitly does NOT care about:

- gRPC stream survival across reload (TUI streams reconnecting after daemon reload is fine).
- Daemon-managed background subprocess survival (the live-sessions feature is broken and unused anyway).
- Cross-worker stream replay.
- The supervisor multiplexing gRPC for the public daemon socket.

---

## 4. What CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) was, and why I built it (and why I shouldn't have)

CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) was a multi-phase architectural change designed by me without explicit user approval for everything it included.

### What CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) designed

A **supervisor-binds-public-listener** model. Instead of the worker binding the public daemon Unix socket (`daemon.sock`) and running its own `*grpc.Server`, the supervisor binds it. The supervisor runs a `*grpc.Server` on the public socket that **forwards** every gRPC call to whichever worker is currently active.

The forwarding is implemented via a custom wire protocol called `supproto` (defined in `internal/supproto/`). Frame types include `Hello`, `UnaryRequest`, `UnaryResponse`, `StreamSubscribe`, `StreamData`, `StreamCancel`, `StreamPush`, etc. The supervisor runs:

- `internal/clydesup/grpc_relay.go` `GRPCRelay`. Handles streaming RPCs by sending `StreamSubscribe` frames to the worker and forwarding `StreamData` frames back to the gRPC client. Has a per-stream replay ring (`StreamRing`) so streams survive worker swap.
- `internal/clydesup/unary_proxy.go` `UnaryForwardingProxy`. Handles unary RPCs. 30 forwarder methods, one per RPC, each marshaling the request, calling `WorkerClient.CallUnary`, unmarshaling the response.
- `internal/clydesup/grpc_server.go` `GRPCServer`. The composite that registers `UnimplementedClydeServiceServer` and overrides everything with relay/proxy methods.
- `internal/clydesup/stream_swap.go` `WorkerRegistry`, `StreamRegistry`, `StreamRing`. Tracks active worker, holds replay buffers.

The worker runs `internal/daemon/worker_relay_server.go` `WorkerRelayServer` on a per-generation Unix socket (`daemon.worker.<gen>.sock`). It accepts the supervisor's connection, dispatches incoming frames:

- `unary_request` goes through `worker_relay_unary.go` looking up the method in a static dispatch table of 30 entries, calling the typed handler on the local `*Server`, sending back `unary_response`.
- `stream_subscribe` goes through `worker_relay_stream.go` looking up the streaming method in a dispatch table of 6 entries, running the real handler, converting each `Send` into a `stream_data` frame.

Phases of CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h):

- **Phase 1** = CLYDE-300 (live-runtime survival on reload via Setsid spawn + detach-only closers + filesystem manifest reattach for daemon-managed background subprocesses) = "live runtime survival on reload" (Setsid spawn + detach-only closers + filesystem manifest reattach for daemon-managed background subprocesses).
- **Phase 2** = CLYDE-301 (supervisor owns subprocesses; supervisor spawns Codex app-server and Claude remote worker rather than the worker doing it) = "supervisor owns subprocesses" (supervisor spawns Codex app-server and Claude remote worker, not the worker).
- **Phase 3** = CLYDE-302 (supervisor owns capture writers; SupervisorCaptureSink ships capture lines to supervisor over a custom wire protocol, supervisor owns the lumberjack rotated writers) = "supervisor owns capture writers" (`SupervisorCaptureSink` ships capture lines over supproto to the supervisor, supervisor owns lumberjack writers).
- **Phase 4** = CLYDE-303 (gRPC stream relay through supervisor; multiplexes streaming RPCs across worker generations using replay buffers so streams survive worker swap) = "gRPC stream relay through supervisor" (the GRPCRelay above).
- **Sub-phases** 9a through 9h = the bottom-up implementation steps for the supervisor architecture.

### Why I built it

I conflated the user's two stated requirements (MITM survival + Codex/Claude survival) into a single "zero-disconnect-reload" goal. The user did say "I am pretty sick and tired of disconnects during dev" and "100% please". That gave me what I treated as authorization for the whole architecture. In reality the user was talking about the MITM + wrapper symptoms, not asking for "zero disconnect across all RPCs".

### Why CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) has to go

1. **The supervisor-relay produced a stringly-typed dispatch bug.** The supervisor sent bare method names like `"SubscribeRegistry"`. The worker dispatch table was keyed by full proto names like `/clyde.v1.ClydeService/SubscribeRegistry`. The lookup failed silently. Every streaming RPC except `StreamLiveSession` returned "unknown streaming method" at the worker. This broke the TUI's dashboard streams entirely.
2. **The supervisor ownership of the public listener added a forwarding hop for every CLI RPC.** This created multiple new failure modes (worker not yet registered when supervisor accepts connection, stale generation hello races, half-closed worker sockets during swap window).
3. **Multiple deploys failed because of it.** The harmony deploy on 2026-05-09 broke `clyde` launch entirely (worker died 3 seconds after start; no active worker; CLI calls returned `Unavailable`). Required full rollback to `cb9fc7d`.
4. **The architectural defense Go can't provide.** Go's type system can't enforce "this string must come from the proto-generated `_FullMethodName` constants". The fix would be a typed wrapper struct or a static analyzer (filed as separate work in go-makefile). Either way, the underlying bug class is inherent to the supervisor-relay shape we adopted.
5. **It was solving problems the user doesn't have.** Live session subprocess survival? User doesn't use live sessions. TUI stream survival across reload? User wants the TUI to be resilient, not the streams to survive. The MITM cascade? Solved by Fix 1 alone (a much smaller change).

---

## 5. The minimal fix: what's actually needed

### Fix 1 (the only real fix the user asked for)

**Mechanism**: the supervisor binds the MITM TCP listener (`[::1]:48723`) at startup. It holds the file descriptor for the entire daemon lifetime. When it spawns a worker, it `dup`s the FD into the worker via `cmd.ExtraFiles`. The worker calls `net.FileListener` on the inherited FD and serves on it.

**Why this gives zero bind-gap**: the kernel-level TCP listener never closes. OLD worker has a `dup` of the FD. NEW worker has its own `dup` of the same kernel FD. NEW worker can start accepting on its `dup` BEFORE OLD worker drops its `dup`. There's never a moment when no process is accepting.

**Why FD inheritance alone (cb9fc7d's current model) might not be enough**: cb9fc7d already passes the MITM listener FD between OLD and NEW workers. But the OLD worker's MITM proxy `Shutdown` calls `listener.Close()` at some point. If `Close()` happens before the NEW worker has started accepting on its inherited FD, there's a brief gap.

**Open question (CLYDE-319 (Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected; investigation prerequisite for the revert) in Tack)**: does cb9fc7d actually have a bind-gap, or is the FD inheritance tight enough that there's no gap? If the latter, Fix 1 isn't needed. We just revert CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) and we're done. The next LLM should investigate this BEFORE writing Fix 1 code.

### What the source for Fix 1 looks like

Fix 1 already exists in the worktree `clyde-fix-mitm-supervisor` at branch `fix-mitm-listener-supervisor` commit `736ceb5`. It was authored against the CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) supervisor (which is much bigger than cb9fc7d's). To use it on cb9fc7d, the next LLM has to re-author it on the cb9fc7d supervisor (the small one). The mechanism is the same. The surrounding code is different.

Specific functions in the golden version that need to be ported:

- `internal/daemon/supervisor.go:261-298` `listenSupervisorMITM()`. Binds the TCP listener.
- `internal/daemon/supervisor.go:637-676` `initialWorkerListenerInheritance()`. Builds the `cmd.ExtraFiles` and env entries for spawn.
- `internal/daemon/supervisor.go:898-961` `injectSupervisorMITMHandoff()`. For reload-time handoff.
- `internal/daemon/run.go`. Worker side: `listenerNameMITM` constant, `loadInheritedListeners()` reads the inherited FD, `startMITM()` uses the inherited listener if present.

Tests to port: `internal/daemon/mitm_supervisor_handoff_test.go` (294 lines).

### What is explicitly NOT being fixed in CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about)

- gRPC stream survival (CLYDE-303 (gRPC stream relay through supervisor; multiplexes streaming RPCs across worker generations using replay buffers so streams survive worker swap)). Not a user requirement.
- Daemon-managed subprocess survival via manifest reattach (CLYDE-300 (live-runtime survival on reload via Setsid spawn + detach-only closers + filesystem manifest reattach for daemon-managed background subprocesses)/301). Protects a broken/unused feature.
- Supervisor-owned capture writers (CLYDE-302 (supervisor owns capture writers; SupervisorCaptureSink ships capture lines to supervisor over a custom wire protocol, supervisor owns the lumberjack rotated writers)). Independent of MITM survival.
- Hello-coupled worker readiness (Fix 2). Was needed only to make CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) work.
- Gen-monotonic guard (Fix 3). Same.
- Idiomatic readiness refactor (`d0f3bc0`). Refactor on top of Fix 2.

---

## 6. Current world state

**Date**: 2026-05-09 evening PT.

### Git state


| Ref                                 | Commit    | Meaning                                                                                                                                                                                                                                                                     |
| ----------------------------------- | --------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `main`                              | `cb9fc7d` | Pre-harmony, known-good. Deployed.                                                                                                                                                                                                                                          |
| `golden/zdr-restored`               | `50b60f9` | All CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) work + fixes. **Do NOT deploy.** |
| `fixup/zdr-broken`                  | `9edbac0` | Older harmony+phase1 snapshot from earlier rollback. Historical.                                                                                                                                                                                                            |
| `golden/zdr-harmony`                | `b27335c` | The harmony attempt that broke. Historical.                                                                                                                                                                                                                                 |
| `backup/main-pre-harmony`           | `cb9fc7d` | Backup ref pointing at current main.                                                                                                                                                                                                                                        |
| `backup/main-pre-clyde-270`         | `9a2f715` | Earlier baseline. Historical.                                                                                                                                                                                                                                               |
| `backup/main-pre-d4e5e4b`           | `d4e5e4b` | Earlier rollback target. Historical.                                                                                                                                                                                                                                        |
| `backup/main-pre-lint-merge`        | `d690dc5` | Earlier rollback target. Historical.                                                                                                                                                                                                                                        |
| `backup/main-pre-livetrack-batch-2` | `1290b3c` | Earlier baseline. Historical.                                                                                                                                                                                                                                               |
| `backup/main-pre-161-271-merge`     | `87d81e1` | Earlier baseline. Historical.                                                                                                                                                                                                                                               |


### Daemon state

```
1063  /Users/agoodkind/.local/bin/clyde daemon         (supervisor)
1129  /Users/agoodkind/.local/bin/clyde daemon worker  (worker)
```

Running `cb9fc7d`. Healthy. launchd-managed via `~/Library/LaunchAgents/io.goodkind.clyde.daemon.plist`.

### All worktrees (90 total)

Format: path, SHA, branch, description.

**Active main checkout:**

- `/Users/agoodkind/Sites/clyde-dev/clyde` `cb9fc7d` `[main]`. Current main worktree.

**Codex worktrees (separate tool, not Clyde-dev):**

- `/Users/agoodkind/.codex/worktrees/2c93/clyde` `0252f53` (detached). Codex's own worktree, ignore.
- `/Users/agoodkind/.codex/worktrees/8ea0/clyde` `a8b28ed` (detached). Same.
- `/Users/agoodkind/.codex/worktrees/b26d/clyde` `6f4d3f9` (detached). Same.

**The "golden" reference (do NOT delete; needed for cherry-picks):**

- `/Users/agoodkind/Sites/clyde-dev/clyde-golden-restored` `50b60f9` `[golden/zdr-restored]`. Full CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) + all fixes, gates green, but DASHBOARD STREAMS BROKEN due to stringly-typed dispatch bug. Source for Fix 1 cherry-pick. Drop after CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) lands.
- `/Users/agoodkind/Sites/clyde-dev/clyde-zdr-harmony` `b27335c` `[golden/zdr-harmony]`. The broken harmony attempt. Historical.

**CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) phase work (all abandoned; CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) will revert these out):**

- `/Users/agoodkind/Sites/clyde-dev/clyde-fix-mitm-supervisor` `736ceb5` `[fix-mitm-listener-supervisor]`. Fix 1 reference implementation. Will be re-authored for cb9fc7d.
- `/Users/agoodkind/Sites/clyde-dev/clyde-fix-prehandover` `ffbe2cf` `[fix-worker-socket-prehandover]`. Fix 3, abandoned.
- `/Users/agoodkind/Sites/clyde-dev/clyde-fix-worker-readiness` `4843d98` `[fix-worker-readiness-hello]`. Fix 2, abandoned.
- `/Users/agoodkind/Sites/clyde-dev/clyde-readiness-idiomatic` `d0f3bc0` `[fix-readiness-idiomatic]`. Readiness refactor, abandoned.
- `/Users/agoodkind/Sites/clyde-dev/clyde-phase-4b` `502155a` `[clyde-308-phase-4b]`. CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) 9d/9e WIP, abandoned.
- `/Users/agoodkind/Sites/clyde-dev/clyde-phase-4b-fmn` `e4c79a1` `[phase-4b-supervisor-fullmethodname]`. Supervisor uses FullMethodName fix, abandoned.
- `/Users/agoodkind/Sites/clyde-dev/clyde-phase-4b-test-rs` `502155a` `[phase-4b-test-registry-stats]`. Test for SubscribeRegistry/ProviderStats, abandoned. Has uncommitted file `internal/daemon/relay_subscribe_registry_test.go` documenting the stringly-typed bug.
- `/Users/agoodkind/Sites/clyde-dev/clyde-phase-4b-test-rs2` `b456a6c` `[phase-4b-test-reload-survival]`. SubscribeRegistry reload-survival test, abandoned.
- `/Users/agoodkind/Sites/clyde-dev/clyde-phase-4b-test-tc` `502155a` `[phase-4b-test-transcript-compact]`. Test for TailTranscript/CompactPreview/CompactApply, abandoned. Has uncommitted file `internal/daemon/relay_transcript_compact_test.go`.
- `/Users/agoodkind/Sites/clyde-dev/clyde-zdr-9a` `97e9cb7` `[clyde-308-9a]`. CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) phase 9a (supproto frames + WorkerClientConn), abandoned.
- `/Users/agoodkind/Sites/clyde-dev/clyde-clyde-299` `82a684b` `[fix-clyde-299-lumberjack]`. CLYDE-299 (lumberjack flock contention fix; per-Proxy capture writer cache + ErrCaptureSinkClosed sentinel; lives on clyde-clyde-299 worktree at commit 82a684b) fix (per-Proxy capture writer cache + ErrCaptureSinkClosed sentinel). Could be backported to main as standalone (see CLYDE-299 (lumberjack flock contention fix; per-Proxy capture writer cache + ErrCaptureSinkClosed sentinel; lives on clyde-clyde-299 worktree at commit 82a684b) Tack comment).

**Codex's earlier rearch attempts (abandoned):**

- `/Users/agoodkind/Sites/clyde-dev/clyde-phase2-daemon-lifecycle-v2` `b27335c` `[phase2-daemon-lifecycle-v2]`. Codex's earlier attempt at daemon lifecycle refactor.
- `/Users/agoodkind/Sites/clyde-dev/clyde-phase2-daemon-lifecycle-v3` `9edbac0` `[phase2-daemon-lifecycle-v3]`. Same.
- `/Users/agoodkind/Sites/clyde-dev/clyde-rearch-daemon-lifecycle-core` `b27335c` `[rearch-daemon-lifecycle-core]`. Older rearch attempt.
- `/Users/agoodkind/Sites/clyde-dev/clyde-rearch-daemon-status-ux` `b27335c` `[rearch-daemon-status-ux]`. Older rearch attempt.

**TUI / UX / session work (independent of CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h); some land on main, some abandoned):**

- `/Users/agoodkind/Sites/clyde-dev/clyde-fix-tui-bundle` `14a5299` `[fix-tui-ux-bundle]`. TUI debounce + transcript loader fixes. Worth backporting to main (see CLYDE-312 (Backport TUI debounce + transcript loader fixes from golden commit 64d2238 to main; daemon-offline badge debounce 500ms, name column max width cap)).
- `/Users/agoodkind/Sites/clyde-dev/clyde-session-tests` `da0138f` `[backfill-session-store-tests]`. Session store test backfill. Could be backported to main as standalone.
- `/Users/agoodkind/Sites/clyde-dev/clyde-session-ownership` `cb9fc7d` `[fix-session-store-ownership]`. Investigation only, no commits.

**Other worktrees:**

- `/Users/agoodkind/Sites/clyde-dev/clyde-anth-system-prefix` `8b66fa1` `[validate-anth-system-prefix]`. Anthropic system-prefix validation tests, may already be merged.
- `/Users/agoodkind/Sites/clyde-dev/clyde-auto-rename` `051a37f` `[add-auto-rename]`. Auto-rename worker A.
- `/Users/agoodkind/Sites/clyde-dev/clyde-autoname-pr1-data-model` `b7b3ea5` `[clyde-170-pr1-data-model]`. Autoname PR1.
- `/Users/agoodkind/Sites/clyde-dev/clyde-247` `ff28e7f` `[clyde-247]`. Old work.
- `/Users/agoodkind/Sites/clyde-dev/clyde-adapter-retries` `ae2b211` `[implement-adapter-retries]`. Adapter retries, status unknown.
- `/Users/agoodkind/Sites/clyde-dev/clyde-codex-retry-fix` `0252f53` `[fix-codex-overloaded-retry-tests]`. Old work.
- `/Users/agoodkind/Sites/clyde-dev/clyde-context-state-cache` `0252f53` `[clyde-context-state-cache]`. Old work.
- `/Users/agoodkind/Sites/clyde-dev/clyde-cursor-manual-mitm-wrapper` `12cca9a` `[golden/cursor-manual-mitm-wrapper]`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-cursor-mitm-path-fix` `c288225` `[cursor-mitm-path-fix]`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-daemon-status-guard` `0a62e93` `[fix-daemon-status-guard]`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-deploy-ownership` `0252f53` `[clyde-deploy-ownership]`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-docs-agents` `b67cd58` `[docs-agents-md]`. AGENTS.md sections from this session, may be useful.
- `/Users/agoodkind/Sites/clyde-dev/clyde-error-shape-research` `b19a45a`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-errorbody-alias-lint` `e6707ab`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-full-cursor-wrapper` `2c7b629`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-launchd-supervisor` `0252f53`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-lint-anthropic-backend` `f70ed09`. Old lint fixes, may already be merged.
- `/Users/agoodkind/Sites/clyde-dev/clyde-lint-cli-mitm` `b2ee9ed`. Same.
- `/Users/agoodkind/Sites/clyde-dev/clyde-lint-codex-runtime` `f94569c`. Same.
- `/Users/agoodkind/Sites/clyde-dev/clyde-lint-cursor-deadcode` `928e24c`. Same.
- `/Users/agoodkind/Sites/clyde-dev/clyde-lint-mcpserver` `cae486f`. Same.
- `/Users/agoodkind/Sites/clyde-dev/clyde-lint-session-ui` `3c707fb`. Same.
- `/Users/agoodkind/Sites/clyde-dev/clyde-log-inventory` `c288225`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-logpolicy-config` `c288225`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-logpolicy-mitm` `c288225`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-logpolicy-slogger` `c288225`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-merge-lint-fallout` `d690dc5`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-mitm-concerns` `0bf52bf`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-mitm-wrapper-electron-helpers` `c34ede0`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-reviewer-prompt` `da89e1c` `[docs-reviewer-prompt]`. Saved reviewer prompt doc, useful as reference.
- `/Users/agoodkind/Sites/clyde-dev/clyde-supervised-reload` `0252f53`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-terminal-cleanup` `353acaa`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-tui-render-nonblocking` `4ff1ae1`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-ui-tcell-lint` `c6daaba`. Old.
- `/Users/agoodkind/Sites/clyde-dev/clyde-worker-b-daemon-mitm` `4ff1ae1`. Old worker.
- `/Users/agoodkind/Sites/clyde-dev/clyde-worker-c-mitm-capture-bounds` `4ff1ae1`. Old worker.
- `/Users/agoodkind/Sites/clyde-dev/clyde-worker-d-hotpath-validation` `4ff1ae1`. Old worker.
- `/Users/agoodkind/Sites/clyde-dev/worktrees/clyde-`* (~25 worktrees under `worktrees/` subdir). Older session-name-refactor work, all abandoned, all candidate for prune in CLYDE-318 (Worktree + branch cleanup after the revert lands; ~50 abandoned worktrees and ~20 branches need pruning).

### Other repos

- `/Users/agoodkind/Sites/go-makefile`. Separate repo containing the project's reusable lint pipeline (staticcheck-extra analyzers, golangci-lint config, Makefile bootstrap). The user has their own go-makefile agent that handles merges there. **DO NOT touch unless explicitly asked.** Currently on branch `add-rta-slog-field-bypass` at `48a50fb`.
- `/Users/agoodkind/Sites/go-makefile-grpc-detector`. Worktree off go-makefile main, contains the in-progress `grpc_method_name_literal` analyzer (uncommitted, agent was stopped mid-task). For user review.
- `/Users/agoodkind/Sites/cursor-via-clyde-wrapper`. Separate Swift+AppKit repo for the native macOS Cursor wrapper. At `0990324`. Production cutover already done. Bundle installed at `~/Applications/Cursor (via clyde).app`.

---

## 7. Tack tickets (the canonical work tracker)

Tack is the issue tracker (similar to Linear/Jira). Workspace `main`, project `CLYDE`. States: `Backlog`, `Todo`, `In Progress`, `Done`, `Cancelled`.

### New tickets filed for CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) revert and follow-ups


| ID                                                                                                                                                                 | Priority | Title                                                                                                                                                                                                                                                                                                              |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about)**             | urgent   | Revert CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) supervisor-relay machinery; keep Fix 1 MITM listener supervisor only |
| **CLYDE-310 (Codex wrapper has no daemon-monitor goroutine; parity with Claude wrapper which polls daemon every 30s)**                                             | medium   | Codex wrapper has no daemon-monitor goroutine; parity with Claude wrapper                                                                                                                                                                                                                                          |
| **CLYDE-311 (Investigate TUI parent-of-CLI relationship; harden TUI resilience to brief daemon offline so it does not kill child Claude/Codex sessions)**          | high     | Investigate TUI parent-of-CLI relationship and harden TUI resilience to daemon offline                                                                                                                                                                                                                             |
| **CLYDE-312 (Backport TUI debounce + transcript loader fixes from golden commit 64d2238 to main; daemon-offline badge debounce 500ms, name column max width cap)** | high     | Backport TUI debounce + transcript loader fixes from golden 64d2238 to main                                                                                                                                                                                                                                        |
| **CLYDE-313 (Dashboard live-session feature does not work and user does not use it; decide whether to remove or fix)**                                             | low      | Dashboard live-session feature does not work; remove or fix                                                                                                                                                                                                                                                        |
| **CLYDE-314 (Document STATICCHECK_EXTRA_NOOP_CLOSER_ALLOWLIST in AGENTS.md so future agents see the allowlist mechanism for legitimate no-op closers)**            | medium   | Document STATICCHECK_EXTRA_NOOP_CLOSER_ALLOWLIST in AGENTS.md                                                                                                                                                                                                                                                      |
| **CLYDE-315 (Slog ctx-threading TODOs in internal/providers/codex/store; 6 sites where slog calls cannot easily get a context)**                                   | low      | Slog ctx-threading TODOs in internal/providers/codex/store                                                                                                                                                                                                                                                         |
| **CLYDE-316 (Triage flaky daemon parallel tests TestApplyAutoRename and TestForegroundLease; race only under parallel make test, pass in isolation)**              | low      | Triage flaky daemon parallel tests (TestApplyAutoRename, TestForegroundLease)                                                                                                                                                                                                                                      |
| **CLYDE-317 (Triage flaky TestRunAutoRenamePassAppliesToCodexSession; flakes once and passes on retry)**                                                           | low      | Triage flaky TestRunAutoRenamePassAppliesToCodexSession                                                                                                                                                                                                                                                            |
| **CLYDE-318 (Worktree + branch cleanup after the revert lands; ~50 abandoned worktrees and ~20 branches need pruning)**                                            | low      | Worktree + branch cleanup after CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) revert lands                                                                                                                    |
| **CLYDE-319 (Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected; investigation prerequisite for the revert)**              | high     | Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected                                                                                                                                                                                                                         |


### Tickets with comments noting they're being reverted by CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about)

These are now superseded. State should likely move to Cancelled (the user can do this; agents shouldn't change Tack state without explicit direction):


| ID                                                                                                                                                                                                                                     | Title                                | Note                                                           |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------ | -------------------------------------------------------------- |
| CLYDE-300 (live-runtime survival on reload via Setsid spawn + detach-only closers + filesystem manifest reattach for daemon-managed background subprocesses)                                                                           | live-runtime survival on reload      | Likely reverted; protected broken/unused live-sessions feature |
| CLYDE-301 (supervisor owns subprocesses; supervisor spawns Codex app-server and Claude remote worker rather than the worker doing it)                                                                                                  | supervisor owns subprocesses         | Reverted                                                       |
| CLYDE-302 (supervisor owns capture writers; SupervisorCaptureSink ships capture lines to supervisor over a custom wire protocol, supervisor owns the lumberjack rotated writers)                                                       | supervisor owns capture writers      | Reverted                                                       |
| CLYDE-303 (gRPC stream relay through supervisor; multiplexes streaming RPCs across worker generations using replay buffers so streams survive worker swap)                                                                             | gRPC stream relay through supervisor | Reverted                                                       |
| CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) | (architecture umbrella)              | Reverted                                                       |
| CLYDE-299 (lumberjack flock contention fix; per-Proxy capture writer cache + ErrCaptureSinkClosed sentinel; lives on clyde-clyde-299 worktree at commit 82a684b)                                                                       | lumberjack flock contention fix      | Could be backported standalone from clyde-clyde-299 worktree   |


### Tickets unrelated to CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) (still relevant)

- **CLYDE-298 (Codex CLI append-only compact support; feature ticket unrelated to the revert)**. Codex CLI append-only compact support. Unrelated, still Todo.
- **CLYDE-263 (clyde mitm show CLI debug subcommand displaying captured HTTPS sessions)**. `clyde mitm show` CLI (already shipped on cb9fc7d).
- **CLYDE-261 (Anthropic signature_delta plumbing; previously shipped), 262, 266, 267, 269, 270-277**. Older work, mostly shipped on cb9fc7d.

### Tack tools

To file a ticket: `mcp__tack__tack_create_issue` with `workspace_reference="main"`, `project_reference="CLYDE"`, `name=...`, optional `properties.description` and `properties.priority`.
To comment on a ticket: `mcp__tack__tack_create_comment` with `issue_reference="CLYDE-N"`, `name=...`, `properties.description=...`.
To change state: `mcp__tack__tack_set_issue_state` with `state="CLYDE::Done"` or similar.

The user explicitly said baseline files are user-only. Agents do not touch them and do not file tickets that propose baseline edits. See behavioral rules (section 11).

---

## 8. The CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) revert plan

### Strategy

**Forward-pick onto cb9fc7d.** A fresh branch off cb9fc7d. Replay only the changes we want. Each commit is small, individually reviewable, leaves the tree buildable.

Rejected alternative: backward-strip from `golden/zdr-restored`. Rejected because intermediate commits would not build (the tree has 4500+ lines of relay code that the worker depends on indirectly). Rejected because every commit would be a multi-thousand-line negative-space diff. Rejected because tests written against CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) would have to be deleted in motion.

### Worktree

Create `/Users/agoodkind/Sites/clyde-dev/clyde-revert-308` on branch `revert/clyde-308`, off cb9fc7d. Leave `clyde-golden-restored` in place as the cherry-pick source. Leave the four `clyde-phase-4b`* and `clyde-zdr-9a` worktrees untouched. They will be abandoned. Branch deletion is CLYDE-318 (Worktree + branch cleanup after the revert lands; ~50 abandoned worktrees and ~20 branches need pruning).

### Commit sequence

Each commit ends with `make build`, `make test`, `make staticcheck-extra`, `make deadcode`, `make govulncheck`, and `make fmt && git diff --exit-code` clean. Baseline files NEVER touched. If a baseline edit would be required, stop and report.

**Prerequisite (from CLYDE-319 (Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected; investigation prerequisite for the revert))**: investigate whether cb9fc7d already has zero-bind-gap via FD inheritance or actually has a bind-gap. If FD inheritance is sufficient, the entire revert reduces to "do nothing on main, just verify". If not, proceed with commits below.

**Commit 1 (if needed): Add supervisor-owned MITM listener.**

Touches `internal/daemon/supervisor.go`:

- Add `listenSupervisorMITM()` (port from golden's `supervisor.go:261-298`).
- Add `openSupervisorListeners()` and `closeSupervisorListeners()` to wrap the existing `controlListener` plus the new MITM listener in a `supervisorListeners` bundle.
- Add `initialWorkerListenerInheritance()` (port from golden's `supervisor.go:637-676`) that builds the `cmd.ExtraFiles` slice and the `CLYDE_DAEMON_INHERITED_LISTENERS` env entry naming `mitm` at fd 3.
- Add `injectSupervisorMITMHandoff()` (port from golden's `supervisor.go:898-961`) so the replacement worker on reload also gets the dup'd MITM fd alongside the readiness writer.
- Add `closeMergedSupervisorMITMDup()` for cleanup.

Touches `internal/daemon/run.go`:

- Add `listenerNameMITM` constant.
- In `loadInheritedListeners()` (cb9fc7d `run.go:581-616`), recognize the `mitm` listener spec.
- In `startMITM()` (cb9fc7d `run.go:1505` neighborhood), prefer the inherited listener when present.
- In `inheritedListenerFiles` and `drainReloadedMITM`, gate the MITM-listener-close path on "supervised" (env var presence). Mirror golden's `736ceb5` change.

Darwin-only. Non-darwin path unchanged.

**Commit 2 (if needed): Add Fix 1 tests.**

Port `internal/daemon/mitm_supervisor_handoff_test.go` from golden verbatim (294 lines). Verifies:

- `injectSupervisorMITMHandoff` rejects duplicate worker specs.
- `mitmFD` is computed correctly as `3 + listenerCount`.
- `closeMergedSupervisorMITMDup` closes only the supervisor-side dup.

**Commit 3 (if needed): Update existing reload tests to tolerate supervised MITM path.**

Touches `internal/daemon/mitm_reload_test.go` and `internal/daemon/mitm_reload_drain_test.go`. Mirrors golden's edits in `736ceb5` (skip `closeListener` when supervised).

**Commit 4: Documentation update.**

Edit `AGENTS.md` to add a single short section: "Supervisor owns MITM TCP listener on darwin; worker binds the public daemon socket; the supervisor never closes the MITM listener FD across worker generations, eliminating bind-gap-induced upstream cascade failures." Remove or correct any golden-flavored "Supervisor binds public listeners" text if it leaked into main.

### Verification gate (after final commit)

Per-commit gate (already listed): build, test, lint, staticcheck-extra, deadcode, govulncheck, fmt-clean.

End-to-end gate, on darwin hardware:

(a) **MITM listener zero-bind-gap.** Run `clyde claude` (or any CLI consumer pointed at the MITM). Hold a long-running streaming request open. Run `clyde daemon reload` mid-stream. Assert the upstream CLI does not surface "reconnecting 1/N" messages, the streaming request completes successfully, no upstream session is permanently broken.

(b) **gRPC client clean retry on reload.** Subscribe a TUI stream. Reload. Assert: stream sees `EOF` or `Unavailable`, TUI debounce hides the badge for ~500ms (after CLYDE-312 (Backport TUI debounce + transcript loader fixes from golden commit 64d2238 to main; daemon-offline badge debounce 500ms, name column max width cap) lands), client retries within 1s, new stream established. NOT survival. Clean disconnect-and-retry. Regression coverage that we did not introduce a hang.

(c) **Adapter and webapp listeners survive reload.** `curl` the adapter at `[::1]:11434` before and after reload. Confirm no bind gap. Unchanged from cb9fc7d.

### Risks

- **MITM listener fd cleanup on supervisor death**. cb9fc7d does not have it. Without `closeSupervisorListeners`, a launchd respawn after supervisor crash would fail to re-bind the MITM port. Commit 1 must add this.
- **Reload-control protocol** differs between cb9fc7d and golden in fd slot positioning. Commit 1 must verify the worker reads the dup'd MITM fd at the correct slot. Off-by-one would silently fall through to local rebind.
- **launchd plist target**. Must be verified to point at the supervisor entrypoint (`daemon`), not directly at the worker. cb9fc7d should already be correct here. Verify before deploy.
- **Tests green on golden because of CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) may fail on revert**. Run `make test ./internal/daemon/...` after each commit, not just at the end.
- **Phase 4b WIP worktrees** stay in place during the revert. They cannot land. Branch deletion is CLYDE-318 (Worktree + branch cleanup after the revert lands; ~50 abandoned worktrees and ~20 branches need pruning).
- **Live session subsystem stays broken** (CLYDE-313 (Dashboard live-session feature does not work and user does not use it; decide whether to remove or fix)). This revert does not fix it. It already does not work today; the revert leaves it in the same non-working state.

### What CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) explicitly does NOT touch

- `internal/livetrack/` (CLYDE-270 (livetrack primitive itself; lifecycle-tracking registry under internal/livetrack used to drain in-flight sessions on reload; predates the revert) lifecycle-tracking primitive, stands).
- MITM correlation tooling, `clyde mitm show` (CLYDE-263 (clyde mitm show CLI debug subcommand displaying captured HTTPS sessions) stands).
- OAuth, MCP, pruning, session store.
- AGENTS.md sections other than the small Fix 1 description.
- TUI debounce backport (CLYDE-312 (Backport TUI debounce + transcript loader fixes from golden commit 64d2238 to main; daemon-offline badge debounce 500ms, name column max width cap), separate ticket).
- CLYDE-299 (lumberjack flock contention fix; per-Proxy capture writer cache + ErrCaptureSinkClosed sentinel; lives on clyde-clyde-299 worktree at commit 82a684b) lumberjack flock fix backport (independent, not part of this revert).
- Session store single-ownership refactor (independent).
- go-makefile baselines (NEVER edited; see behavioral rules).
- The four go-makefile / agent-gate analyzers that exist or are planned (independent, see go-makefile section).

---

## 9. The Tack ticket map for the next LLM

When you (next LLM) sit down to do work, the priority order is:

**Pri 1 (investigation prerequisite)**:

- **CLYDE-319 (Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected; investigation prerequisite for the revert)**. Confirm whether cb9fc7d's MITM listener actually has a bind-gap. If no bind-gap, the revert is a no-op on main and nothing else is needed.

**Pri 2 (the revert itself)**:

- **CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about)**. Execute the revert plan above. Spawn an Opus implementation agent in the `clyde-revert-308` worktree.

**Pri 3 (independent backports that improve UX)**:

- **CLYDE-312 (Backport TUI debounce + transcript loader fixes from golden commit 64d2238 to main; daemon-offline badge debounce 500ms, name column max width cap)**. TUI debounce + transcript loader. Cherry-pick golden's `64d2238`. One commit. Small.
- **CLYDE-310 (Codex wrapper has no daemon-monitor goroutine; parity with Claude wrapper which polls daemon every 30s)**. Codex wrapper daemon-monitor parity. Mirror the Claude wrapper's pattern. One commit.
- **CLYDE-299 (lumberjack flock contention fix; per-Proxy capture writer cache + ErrCaptureSinkClosed sentinel; lives on clyde-clyde-299 worktree at commit 82a684b)** (separate from the closed comment): consider standalone backport of the per-Proxy capture writer cache fix from `clyde-clyde-299` worktree.

**Pri 4 (investigations)**:

- **CLYDE-311 (Investigate TUI parent-of-CLI relationship; harden TUI resilience to brief daemon offline so it does not kill child Claude/Codex sessions)**. TUI parent-of-CLI relationship + resilience hardening. Real failure mode user observed. Read the source carefully.
- **CLYDE-313 (Dashboard live-session feature does not work and user does not use it; decide whether to remove or fix)**. Live-session feature broken; decide remove vs fix.

**Pri 5 (hygiene)**:

- **CLYDE-314 (Document STATICCHECK_EXTRA_NOOP_CLOSER_ALLOWLIST in AGENTS.md so future agents see the allowlist mechanism for legitimate no-op closers)**. Document STATICCHECK_EXTRA_NOOP_CLOSER_ALLOWLIST in AGENTS.md.
- **CLYDE-315 (Slog ctx-threading TODOs in internal/providers/codex/store; 6 sites where slog calls cannot easily get a context)**. codex/store ctx threading TODOs.
- **CLYDE-316 (Triage flaky daemon parallel tests TestApplyAutoRename and TestForegroundLease; race only under parallel make test, pass in isolation)**. flaky daemon parallel tests triage.
- **CLYDE-317 (Triage flaky TestRunAutoRenamePassAppliesToCodexSession; flakes once and passes on retry)**. flaky TestRunAutoRenamePassAppliesToCodexSession triage.
- **CLYDE-318 (Worktree + branch cleanup after the revert lands; ~50 abandoned worktrees and ~20 branches need pruning)**. worktree + branch cleanup after CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about).

---

## 10. Critical code paths and file references

### Daemon entrypoint and reload

- `cmd/clyde/main.go`. Binary entrypoint. Routes to subcommands.
- `cmd/root.go`. Main subcommand dispatcher. Wires TUI callbacks. ~1500 lines.
- `internal/daemon/run.go`. Worker startup, listener binding, FD inheritance, reload orchestration. ~1600 lines.
- `internal/daemon/supervisor.go`. Supervisor process (darwin only). Spawns worker, owns readiness pipe, forwards reload via Unix socket. cb9fc7d: 447 lines. Golden: 1163 lines.
- `internal/daemon/server.go`. The Server struct that implements `clydev1.ClydeServiceServer`. Lots of session/runtime management here. ~3000 lines.

### Reload mechanics (cb9fc7d shape)

- `cb9fc7d run.go:347` `daemonListener` binds `daemon.sock` (worker side).
- `cb9fc7d run.go:618-670` `reloadDaemonBinary` orchestrates reload.
- `cb9fc7d run.go:687-758` `startGracefulGRPCStop` drains old worker.
- `cb9fc7d run.go:1033-1083` `inheritedListenerFiles` collects listener FDs for handoff.
- `cb9fc7d run.go:553-579` `loadInheritedRuntime` decodes inherited listener specs.
- `cb9fc7d run.go:1276-1341` `startMITM` binds the MITM TCP listener (uses inherited if present).
- `cb9fc7d server.go:2701-2721` spawns Claude remote worker via `exec.Command("daemon launch-remote-worker")`. **No Setsid on cb9fc7d.**

### MITM proxy

- `internal/mitm/proxy.go` `Proxy` struct with `captureSink` field. `NewProxy`, `Shutdown`, `shutdownIdle`, `serve`.
- `internal/mitm/capture_policy.go` `WriteCaptureLine` writes JSONL records (cb9fc7d uses package-global cache; golden has per-Proxy cache from CLYDE-299 (lumberjack flock contention fix; per-Proxy capture writer cache + ErrCaptureSinkClosed sentinel; lives on clyde-clyde-299 worktree at commit 82a684b)).
- `internal/mitm/raw_capture.go`. HTTP capture path.
- `internal/mitm/connect_tunnel.go`. HTTPS CONNECT tunnel handling.
- `internal/mitm/ws_capture.go`. WebSocket capture path.

### CLI wrappers (for Concern B / CLYDE-310 (Codex wrapper has no daemon-monitor goroutine; parity with Claude wrapper which polls daemon every 30s))

- `internal/providers/claude/lifecycle/invoke.go`. Claude wrapper. Note `execWrapperProcess = syscall.Exec` at line 34 (replaces the calling process), and `cmd.Run()` at line 582 (fork+exec, blocking).
- `internal/providers/claude/lifecycle/daemon_session.go:54-109`. `monitorDaemon` goroutine (the resilience pattern Codex needs to mirror).
- `internal/providers/claude/lifecycle/pty_invoke.go`. PTY-based Claude invocation for remote control.
- `internal/providers/codex/lifecycle/invoke.go:181-238`. Codex wrapper. **No daemon-monitor goroutine.**
- `internal/providers/codex/lifecycle/appserver.go`. Codex app-server runtime (the live-session subsystem).

### TUI (for CLYDE-311 (Investigate TUI parent-of-CLI relationship; harden TUI resilience to brief daemon offline so it does not kill child Claude/Codex sessions) / CLYDE-312 (Backport TUI debounce + transcript loader fixes from golden commit 64d2238 to main; daemon-offline badge debounce 500ms, name column max width cap))

- `internal/ui/app.go`. Main TUI app (~5000 lines). `App` struct holds callbacks, state, draw logic.
- `internal/ui/tcell_table.go`. Table widget (column widths etc.).
- `internal/ui/tcell_compact_panel.go`, `tcell_details.go`, `tcell_format.go`, `tcell_loading.go`, `tcell_statusbar.go`, `tcell_textbox.go`. Various TUI widget files.
- `internal/ui/livetrack_workers_test.go`. TUI worker registry tests.
- TUI debounce + transcript loader fixes are in golden commit `64d2238` (worktree `clyde-fix-tui-bundle`).

### CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) surface area (the things being deleted in the revert)

- `internal/clydesup/`. The supervisor RPC server package. `grpc_relay.go`, `unary_proxy.go`, `grpc_server.go`, `stream_swap.go`, `worker_client.go`, `server.go`, `registry.go`, `capture_writer.go`, `events.go`, `runtime.go`, `reaper.go`, `spawn*.go`, `flock_unix.go`. **Does not exist on cb9fc7d.**
- `internal/supproto/`. Wire protocol package. `frames.go`, `codec.go`. **Does not exist on cb9fc7d.**
- `internal/daemon/worker_relay_server.go`. Worker-side relay endpoint. **Does not exist on cb9fc7d.**
- `internal/daemon/worker_relay_unary.go`. 30-method unary dispatch table. **Does not exist on cb9fc7d.**
- `internal/daemon/worker_relay_stream.go`. 6-method stream dispatch table. **Does not exist on cb9fc7d.**
- `internal/daemon/supervisor_capture_sink.go`. SupervisorCaptureSink. **Does not exist on cb9fc7d.**
- `internal/daemon/supervisor_client*.go`. SupervisorClient. **Does not exist on cb9fc7d.**

If you find yourself wanting to look at any of these on main, you won't find them. Look at `clyde-golden-restored` worktree instead.

### Live session subsystem (CLYDE-313 (Dashboard live-session feature does not work and user does not use it; decide whether to remove or fix))

- `internal/daemon/live_sessions.go`. The daemon-managed background session machinery (~600 lines).
- `internal/daemon/server.go` `StartLiveSession`, `StreamLiveSession`, `SendLiveSession`, `StopLiveSession` RPCs.
- `internal/ui/app.go` `LiveSessionStartRequest` callback at line ~5138.
- `internal/webapp/server.go` `handleStartLiveSession` HTTP handler.
- `cmd/root.go:175,272`. TUI callback wiring and CLI invocation.

### Documentation in the repo

- `AGENTS.md`. The project's instruction file for agents. Contains layer separation rules, type hygiene, error boundary, daemon reload contract, livetrack adoption rule, and (on golden) the supervisor-binds-listener section that's about to be wrong post-revert.
- `CLAUDE.md`. Claude-specific agent guidance.
- `docs/zero-disconnect-reload-design.md`. The CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h) architecture design doc (on golden, will be deleted in the revert).
- `docs/cursor-mitm-setup.md`. Cursor MITM setup notes.
- `docs/agent-reviewer-prompt.md`. The Opus reviewer prompt (recently saved on `docs-reviewer-prompt` branch).

### Logs

- `~/.local/state/clyde/logs/process/daemon/lifecycle.jsonl`. The live daemon's lifecycle log. Heavy (118MB). Use `tail` and `awk` to filter by timestamp.
- `~/.local/state/clyde/logs/daemon/workers/reload.jsonl`. Reload events.
- `~/.local/state/clyde/logs/daemon/rpc/requests.jsonl` and `streams.jsonl`. gRPC request/stream logs.
- `~/.local/state/clyde/logs/cmd/dispatch.jsonl`. CLI dispatch log.
- `~/.local/state/clyde/mitm/.../capture.jsonl`. Captured HTTPS sessions.
- `~/Library/Logs/clyde-daemon.log`. launchd-routed stdout/stderr.

---

## 11. Behavioral rules (memories from this session)

These are durable rules that apply to all work in this codebase. They were learned through user pushback during this session.

### 11.1 Baseline files are user-only

The user is the SOLE authority on:

- `.staticcheck-extra-baseline.txt`
- `.deadcode-baseline.txt`
- `.golangci-lint-baseline.txt`
- `.golangci.yml` (and any other golangci config)
- Any `*-baseline.txt` file in either the clyde repo or go-makefile repo.

Agents NEVER edit these. Agents NEVER file Tack tickets that propose baseline edits as the fix. If a lint finding pops up, fix the code or use the documented allowlist mechanism (`STATICCHECK_EXTRA_NOOP_CLOSER_ALLOWLIST` Make variable). If neither works, report and stop.

### 11.2 Don't predict Tack ticket numbers

Tack auto-assigns numbers on creation. File first, reference after.

### 11.3 Don't let cruft slip by

When a subagent honestly scopes a fix and leaves residual cruft outside its scope, the agent's framing is correct (scope discipline). The coordinator's framing back to the user must NOT launder that as "fine to keep". The user has been burned by accumulated cruft from prior agent work. Lint laundering, slog-boxing, marker-method tricks, no-op closers, dead code that "stays out of scope". Each one individually defensible. The aggregate is the problem.

When a subagent surfaces "I kept X because it was out of scope", repeat that to the user as cruft, not as a settled state. File a follow-up task immediately with concrete file paths.

### 11.4 Fix root, not symptom

When the structural defense (typed wrapper, lint rule, end-to-end test) is what would have prevented the current bug, that defense IS the fix. Do not file it as a follow-up.

Concrete recurring example: the stringly-typed gRPC method routing bug. The supervisor sent `"SubscribeRegistry"` while the worker keyed on `/clyde.v1.ClydeService/SubscribeRegistry`. The compiler accepted both. The fix is to make callers reference the proto-generated `_FullMethodName` constants AND add a static analyzer that requires it. Both at once. Not the smaller patch with the analyzer "as a follow-up".

### 11.5 Do not conflate distinct concerns

The following are independent layers:

- MITM (HTTPS proxy at `[::1]:48723`).
- OpenAI-compat adapter (at `[::1]:11434`, used by Cursor BYOK).
- TUI to daemon gRPC over Unix socket.
- CLI wrapper to daemon gRPC.

Do not blend them. A "MITM tunnel halts mid-chat" complaint is about the MITM listener, not gRPC streams. A "TUI dashboard subscribe broken" complaint is about gRPC streams, not MITM. The user has pushed back hard when these get mixed.

### 11.6 Read the code; don't pattern-match from grep

Multiple times this session, an agent (or me) referenced a code path it had only grep'd for, not read. The result was wrong claims about behavior. Always read the actual file content before making claims about how a code path works. Do not pattern-match.

### 11.7 Read the file before editing it (Edit tool requirement)

The Edit tool requires a Read first. Do not skip.

### 11.8 git -C is required

Always use `git -C <path> <subcommand>` instead of `cd && git ...` or bare `git ...`. Agent shells in worktrees or subshells often have an unreliable `cwd`. The agent-gate hook enforces this.

### 11.9 No em-dashes in agent prompts or written content

The agent-gate hook blocks em-dashes (the long dash character). Use periods, colons, or semicolons. Even when porting content that contained em-dashes, strip them.

### 11.10 Don't ask permission when the user has already said "yes"

The user is busy. They get frustrated when asked the same question twice. When they've said "yes" or "go ahead", just execute. Stop framing risky things as needing approval again.

### 11.11 Don't ask the user about plan approval via text

In plan mode, only `ExitPlanMode` triggers the plan approval flow. Asking via `AskUserQuestion` or text is wrong.

### 11.12 Tack ticket numbers don't predict; titles don't reference unfiled tickets

Don't write things like "this depends on CLYDE-XXX which I'll file next" because the number is unknown until creation.

### 11.13 Don't propose typed wrappers as the fix when call sites just need to use proto constants

If the underlying problem is "callers used bare strings instead of proto constants", the fix is the call-site change, not a new wrapper type. Go's `type X string` doesn't provide nominal typing for literals (untyped string constants implicitly convert). The right fix combination is: (a) update callers to use the constants, (b) add a static analyzer to enforce.

### 11.14 The user runs a separate go-makefile agent

The `/Users/agoodkind/Sites/go-makefile` repo is the project's reusable lint pipeline. The user has their own dedicated agent that handles merges there. **DO NOT touch go-makefile unless explicitly asked.** Filing tickets in the OSS project for go-makefile work is fine. Making code changes in the repo without explicit user direction is not.

### 11.15 Saved memories live at

- `/Users/agoodkind/.claude/projects/-Users-agoodkind-Sites-clyde-dev/memory/MEMORY.md`. Index of saved memories.
- `/Users/agoodkind/.claude/projects/-Users-agoodkind-Sites-clyde-dev/memory/feedback_baseline_files.md`
- `/Users/agoodkind/.claude/projects/-Users-agoodkind-Sites-clyde-dev/memory/feedback_no_cruft.md`
- `/Users/agoodkind/.claude/projects/-Users-agoodkind-Sites-clyde-dev/memory/feedback_fix_root_not_symptom.md`
- `/Users/agoodkind/.claude/projects/-Users-agoodkind-Sites-clyde-dev/memory/feedback_tack_ticket_numbers.md`
- `/Users/agoodkind/.claude/projects/-Users-agoodkind-Sites-clyde-dev/memory/reference_clyde_config.md`

Read these on session start. They override pattern-matched defaults.

---

## 12. What NOT to do (in addition to behavioral rules)

- Do NOT deploy without explicit user approval and a wind-down chance ("ready to deploy, wind down chats").
- Do NOT merge to main without verification on a golden branch first.
- Do NOT spawn agents for trivial coordinator tasks (a 10-minute Bash sequence is a coordinator task, not an agent task).
- Do NOT propose typed wrappers when a one-line call-site fix suffices.
- Do NOT suggest re-introducing CLYDE-301 (supervisor owns subprocesses; supervisor spawns Codex app-server and Claude remote worker rather than the worker doing it)/302/303/308 ("supervisor relays gRPC") in any form.
- Do NOT conflate MITM listener survival with gRPC stream survival.
- Do NOT assume the user wants any feature beyond what they explicitly stated.
- Do NOT write summary preambles before tool calls. State in one sentence what you're about to do, then do it.
- Do NOT silently fix things the user hasn't authorized; surface them.
- Do NOT hide deferred work as "fine to keep".
- Do NOT touch the four `clyde-phase-4b`* and `clyde-zdr-9a` worktrees except to drop them later (CLYDE-318 (Worktree + branch cleanup after the revert lands; ~50 abandoned worktrees and ~20 branches need pruning)).
- Do NOT touch `clyde-golden-restored` until CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) lands; it's the cherry-pick reference.

---

## 13. In-flight tasks (started but not finished)

- **CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) revert**. Planned but no code committed. The plan is in section 8 above. Worktree `/Users/agoodkind/Sites/clyde-dev/clyde-revert-308` does not exist yet. Needs creation.
- **CLYDE-319 (Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected; investigation prerequisite for the revert) investigation**. Must run before CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) commits. Read `internal/mitm/proxy.go` `Shutdown` on cb9fc7d. Trace whether the listener is closed before NEW worker can FileListener.
- **TUI parent-of-CLI investigation (CLYDE-311 (Investigate TUI parent-of-CLI relationship; harden TUI resilience to brief daemon offline so it does not kill child Claude/Codex sessions))**. Incomplete. Confirmed `internal/providers/claude/lifecycle/invoke.go:34` has `execWrapperProcess = syscall.Exec` and line 582 has `cmd.Run()`. Did NOT trace TUI's `runtime.StartInteractive` to determine if TUI process is the parent of the spawned CLI.
- **Phase 4b WIP commits** (will be discarded by CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) revert):
  - `clyde-phase-4b-test-rs` worktree has uncommitted file `internal/daemon/relay_subscribe_registry_test.go` documenting the stringly-typed bug. Will be deleted with worktree.
  - `clyde-phase-4b-test-tc` worktree has uncommitted file `internal/daemon/relay_transcript_compact_test.go`. Same.
  - `clyde-phase-4b-fmn` at `e4c79a1` had a single fix attempt for the supervisor full-method-name issue. Abandoned.
- **go-makefile grpc_method_name_literal analyzer**. In progress in `/Users/agoodkind/Sites/go-makefile-grpc-detector` worktree. Agent was stopped mid-task. Uncommitted state. Left for the user's go-makefile agent to handle.

---

## 14. Quick-start for the next LLM

If you're picking this up cold:

1. **Read this entire doc.** Don't skim.
2. **Read the saved memory files** at `/Users/agoodkind/.claude/projects/-Users-agoodkind-Sites-clyde-dev/memory/`.
3. **Check current state**:
  ```
   git -C /Users/agoodkind/Sites/clyde-dev/clyde rev-parse --short HEAD     # should be cb9fc7d
   pgrep -afl 'clyde daemon'                                                  # should show 1 supervisor + 1 worker
   git -C /Users/agoodkind/Sites/clyde-dev/clyde worktree list | wc -l       # ~90 worktrees
  ```
4. **Confirm the user's intent**. Ask whether they want to start with CLYDE-319 (Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected; investigation prerequisite for the revert) (the investigation) or jump straight to CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) with the revert.
5. **Tools**: use the Tack MCP tools for ticket management, `Bash` with `git -C` for git, `Read`/`Edit`/`Write` for files, `Agent` to spawn subagents (see below).

### When to spawn subagents

Use the Agent tool with `subagent_type` set to:

- `Explore` for read-only code investigation. Use 1-3 in parallel for broad searches.
- `Plan` for designing implementation approach.
- `general-purpose` for actual code-writing work (use Opus model for complex tasks, sonnet for mechanical work).

When briefing a subagent, include the entire relevant context inline. They don't see this conversation. Reference specific file paths and line numbers. Forbid baseline edits, em-dashes, marker bypasses, no-op closers, etc. up front.

### When NOT to spawn subagents

- Trivial Bash sequences (a `git status` plus a `git log` plus a `cat`).
- Reading 1-2 files.
- Filing 1-3 Tack tickets.
- Single-line code edits.

---

## 15. Open investigations (reproduced from CLYDE-311 (Investigate TUI parent-of-CLI relationship; harden TUI resilience to brief daemon offline so it does not kill child Claude/Codex sessions) + CLYDE-319 (Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected; investigation prerequisite for the revert) for visibility)

These need answering before or during the revert, depending on outcome:

### Investigation A (CLYDE-319 (Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected; investigation prerequisite for the revert), urgent): Does cb9fc7d already have zero-bind-gap?

`internal/daemon/run.go` on cb9fc7d already passes the MITM TCP listener FD between OLD and NEW workers via inheritance. Question: is there a bind-gap during the OLD-worker-Shutdown to NEW-worker-FileListener+Accept window?

How to investigate:

1. Read `internal/mitm/proxy.go` on cb9fc7d. Find `Proxy.Shutdown`. Trace what closes the listener.
2. Read `internal/daemon/run.go::reloadDaemonBinary` on cb9fc7d. Trace the order: when does OLD worker call `proxy.Shutdown`? Is it before or after spawning NEW worker?
3. If OLD worker closes the listener BEFORE NEW worker starts accepting: there's a gap. Fix 1 is needed.
4. If OLD worker keeps the listener open until NEW worker has started accepting (or never closes it because the FD is dup'd into NEW worker): no gap. Fix 1 is unnecessary.

Ideal: write a quick test that opens a TCP listener, dups the FD via `cmd.ExtraFiles`-style mechanism, closes the original, and asserts the dup is still accepting. This proves the kernel-level behavior independent of Clyde's code paths.

### Investigation B (CLYDE-311 (Investigate TUI parent-of-CLI relationship; harden TUI resilience to brief daemon offline so it does not kill child Claude/Codex sessions), high): TUI parent-of-CLI relationship

The user said "TUI is what execs the binary IIRC" and "TUI was crashing claude and codex sessions because it was buggy and wasn't resilient to daemon offline modes".

How to investigate:

1. Trace `runtime.StartInteractive` for both Claude and Codex providers (called from `cmd/root.go:1297` `startNewSessionInDir`).
2. Determine: does the TUI process exec the CLI binary (replacing itself), or fork+exec it (becoming the parent)?
3. Look for daemon-disconnect handlers in `internal/ui/app.go`. Does any handler kill children, panic, or `os.Exit`?
4. Check whether the TUI's signal forwarding (e.g. for SIGINT to forward to child) could propagate a TUI panic to the child.

Files to read:

- `internal/ui/app.go`. Main TUI app.
- `internal/providers/claude/lifecycle/invoke.go` and `pty_invoke.go`.
- `internal/providers/codex/lifecycle/invoke.go`.
- `cmd/root.go` `ForwardToClaudeThenDashboard` (lines 1083-1104), `startNewSessionInDir` (lines 1257-1330).

---

## 16. The MITM env vars consumers use

For your reference (when investigating):

- `HTTPS_PROXY=http://[::1]:48723`. Points the client at the MITM.
- `HTTP_PROXY=http://[::1]:48723`. Same.
- `NO_PROXY=localhost,127.0.0.1,::1`. Exempts local addresses.
- `NODE_EXTRA_CA_CERTS=~/.local/state/clyde/mitm/ca/clyde-mitm-ca.crt`. For Node-based clients (Claude CLI, Cursor).
- `SSL_CERT_FILE=~/.local/state/clyde/mitm/ca/clyde-mitm-ca.crt`. For OpenSSL-based clients.
- `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_URL`. Sometimes set to point at MITM directly (config-dependent).

The MITM CA is generated and persisted at `~/.local/state/clyde/mitm/ca/clyde-mitm-ca.{crt,key}`.

---

## 17. The Cursor wrapper (CLYDE-268 (native macOS Cursor wrapper Swift+AppKit app at /Users/agoodkind/Sites/cursor-via-clyde-wrapper; production cutover already done at commit 0990324))

A separate Swift+AppKit application at `/Users/agoodkind/Sites/cursor-via-clyde-wrapper/`. Bundle ID `io.goodkind.clyde.launcher.cursor`. Installed at `~/Applications/Cursor (via clyde).app`. Production cutover already done (notarized with Apple Developer ID `H3BMXM4W7H`, signed via App Store Connect API key `JHC8GR65Q3`).

The wrapper:

1. Sets MITM env vars (`HTTPS_PROXY`, `NODE_EXTRA_CA_CERTS`, etc.).
2. Sets `--proxy-server` and `--ignore-certificate-errors` Chromium flags.
3. Launches `Cursor.app` with those env vars and flags.
4. Maintains Dock identity, Cmd+Q forwarding, Cmd+M minimize, attention-bounce.

Cursor itself has a `settings.json` at `~/Library/Application Support/Cursor/User/settings.json` that must NOT contain a stale `http.proxy` value. A previous incident (documented in `docs/cursor-mitm-setup.md`) had a hardcoded stale port that overrode the wrapper's `--proxy-server`.

---

## 18. Wrap-up

This document is meant to be read end-to-end before any work begins. If anything in it conflicts with what you find in the actual code, trust the code and update this doc.

The user is exhausted. Multiple deploy cycles have been lost. They want a small, surgical, reviewable revert that gets them back to a working state with the one feature they actually need (zero-bind-gap MITM listener). Anything beyond that is scope they did not request.

Be honest. Read the source. Don't conflate concerns. Don't propose elaborate fixes when a small one will do. Don't hide cruft as "out of scope". When in doubt, ask the user. But don't ask them the same question twice.

Good luck.

---

## 19. Glossary of terms used in this doc

Every concept the doc uses, defined inline. If you see a term in this doc that isn't here, ask.

### agent-gate

A local pre-tool hook installed at `/Users/agoodkind/.local/bin/agent-gate`. It runs before every Bash, Edit, Write, and Agent tool invocation. It rejects writes that contain forbidden patterns (em-dashes, baseline-file edits, bare `git` commands without `-C`, slog-boxing, synthetic marker calls, throwaway service registrations, no-op closer additions, fused-thoughts in prompts). When it rejects, the tool call fails with a structured error message naming the rule violated. The hook is the user's enforcement mechanism for the behavioral rules listed in section 11. You will hit it. Read its error messages carefully and fix the input.

Common rules it enforces:

- `no-fused-thoughts` and `no-fused-thoughts-in-prompts`. Blocks em-dashes (the long dash). Replace with periods, colons, semicolons.
- `git-requires-explicit-C`. Blocks `git status`, `git push`, `git fetch` etc. without `-C <path>`. Use `git -C /path/to/repo <subcommand>`.
- `oss20-block-baseline-bash-tampering`. Blocks any Bash command that looks like it might modify a baseline file.
- Various RTA (Rapid Type Analysis) bypass detectors that flag synthetic call sites added to satisfy deadcode reachability.
- LIFECYCLE001/002. Detects no-op `Close()` methods on types that participate in lifecycle tracking.

### go-makefile

A separate Go repo at `/Users/agoodkind/Sites/go-makefile/`. Contains the project's reusable lint pipeline: `golangci-lint` config, custom `staticcheck-extra` analyzers, Makefile bootstrap, lint baselines. Used by Clyde as `include $(GO_MAKEFILE_DIR)/bootstrap.mk` in Clyde's Makefile. The user has their own dedicated agent for go-makefile work. **DO NOT touch go-makefile unless explicitly asked.**

The custom analyzers in go-makefile's `staticcheck/` directory enforce rules like: no `any` type, no slog-boxing of marker types, no throwaway gRPC service registrations, no synthetic marker method calls, no no-op closers (LIFECYCLE001), no silent error discards in Closer.Close (LIFECYCLE002).

### Subagent types (the Agent tool's `subagent_type` parameter)

When you spawn a subagent via the Agent tool, you pick a type. Available types:

- **Explore**. Read-only search agent. Use it to inventory code, locate files, grep for symbols. It reads excerpts (not whole files) and is bad for cross-file analysis. Specify search breadth: `quick`, `medium`, `very thorough`. Up to 3 in parallel for broad searches.
- **Plan**. Software architect agent. Use it to design implementation strategy. It returns step-by-step plans with critical-file lists and trade-off analysis. Cannot edit or write code.
- **general-purpose**. Full-tool agent. Use it for actual code-writing work. Pass `model: "opus"` for complex tasks (~~30+ minute work) or `model: "sonnet"` for mechanical work (~~5-15 minutes).
- **claude-code-guide**. For Claude Code CLI feature questions. Not for Clyde-specific work.

### Make targets used in Clyde's CI

- `make build`. Runs `go vet`, lint pipeline, `gocyclo`, `deadcode`, `staticcheck-extra`, `govulncheck`, then `go build` and `codesign`. Outputs signed `dist/clyde`.
- `make test`. Runs `go test ./...` with race detection.
- `make lint`. Runs `golangci-lint`, `gocyclo`, `deadcode`, `staticcheck-extra` separately. Reports new findings vs baseline.
- `make staticcheck-extra`. Runs only the custom analyzers from go-makefile.
- `make deadcode`. Runs the `deadcode` Go tool.
- `make govulncheck`. Runs Go's vulnerability database check.
- `make fmt`. Runs `gofumpt` and `goimports`.
- `make deploy`. Runs `make build`, installs binary to `~/.local/bin/clyde`, installs launchd plist, runs `clyde daemon reload`.

The pipeline has lint baselines that exempt pre-existing findings. Agents may not modify baselines (rule 11.1).

### Common Clyde commands

- `clyde daemon`. Starts the daemon (or its supervisor on darwin). Normally invoked by launchd, not directly.
- `clyde daemon reload`. Tells the running daemon to swap its binary in place.
- `clyde daemon status`. Reports the running daemon's PID, build, and uptime.
- `clyde dashboard`. Launches the TUI dashboard. Default action when `clyde` is run with no args in an interactive terminal.
- `clyde claude [args]`. Wraps the underlying `claude` binary with Clyde's session + MITM env. The `args` are forwarded to `claude`. After Claude exits, opens the dashboard if running interactively.
- `clyde codex [args]`. Same shape for Codex.
- `clyde resume <name|id>`. Resumes an existing session.
- `clyde mcp`. Runs an MCP stdio server.
- `clyde mitm show <id>`. Shows captured HTTPS sessions for a given session id.
- `clyde hook sessionstart`. Hook handler invoked by Claude on session start.

### Cursor and Cursor (via clyde)

Cursor is a third-party IDE (`/Applications/Cursor.app`) made by Anysphere. The user runs Cursor through a wrapper called "Cursor (via clyde).app" at `~/Applications/Cursor (via clyde).app`. The wrapper is a Swift+AppKit binary that:

1. Sets MITM env vars on Cursor.
2. Sets `--proxy-server=[::1]:48723` and `--ignore-certificate-errors` Chromium flags.
3. Launches Cursor with those env vars and flags.

The wrapper repo is `/Users/agoodkind/Sites/cursor-via-clyde-wrapper/`. The bundle is signed with Apple Developer ID `H3BMXM4W7H` and notarized via App Store Connect API key `JHC8GR65Q3`.

Cursor has TWO independent ways to talk to LLMs:

1. Its own backend at `api2.cursor.sh` (for Cursor's built-in features like autocomplete, chat with Cursor's own model UX). This is what the MITM intercepts.
2. BYOK ("Bring Your Own Key"). Cursor lets the user configure an OpenAI-compatible endpoint. The user has Clyde's adapter at `[::1]:11434` configured here, so Cursor's BYOK calls go through Clyde's adapter (NOT through MITM).

These are different code paths. Do not conflate.

### Workspace, repo, and Tack project terminology

- **workspace**: the Tack workspace, named `main`. All Tack tickets are filed in this workspace.
- **project** (in Tack): a top-level grouping of tickets. The Clyde project is `CLYDE`. Other projects include `TACK`, `OSS`.
- **repo** (filesystem): a git checkout. `/Users/agoodkind/Sites/clyde-dev/clyde` is the main Clyde repo. `/Users/agoodkind/Sites/go-makefile/` is the go-makefile repo. `/Users/agoodkind/Sites/cursor-via-clyde-wrapper/` is the wrapper repo.
- **worktree**: a git worktree (an additional checkout of the same repo on a different branch). `/Users/agoodkind/Sites/clyde-dev/clyde-`* are worktrees of the Clyde repo. There are 90 of them as of 2026-05-09.

### gRPC concepts as used here

- **Unary RPC**. Single request, single response. Like a function call.
- **Server-streaming RPC**. Single request, stream of responses. Subscribes to events.
- **Client-streaming RPC**. Stream of requests, single response. Not used in Clyde.
- **Bidirectional streaming RPC**. Stream of requests, stream of responses. Used for `StreamLiveSession`.

In CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h)'s relay model, every RPC was forwarded through the supervisor. Streaming RPCs used custom replay buffers to survive worker swap. Unary RPCs used a 30-method dispatch table indexed by full method name.

### Proto-generated full method names

Every gRPC method has a "full method name" in the format `/<package>.<service>/<method>`, e.g., `/clyde.v1.ClydeService/SubscribeRegistry`. The proto generator emits these as constants:

```go
const ClydeService_SubscribeRegistry_FullMethodName = "/clyde.v1.ClydeService/SubscribeRegistry"
```

These constants live in `api/clyde/v1/service_grpc.pb.go`. CLYDE-308 (architecture umbrella for the supervisor-binds-public-listener model with GRPCRelay, supproto wire protocol, WorkerRelayServer per-generation socket, manual stringly-typed dispatch table; spans sub-phases 9a through 9h)'s supervisor was supposed to use these but in some places used bare strings like `"SubscribeRegistry"` instead, causing the dispatch lookup mismatch documented in section 4.

### File descriptor inheritance (FD inheritance)

Unix file descriptors are integers that reference open files (including sockets). When a process forks and execs a new process, the kernel can pass file descriptors to the child via `cmd.ExtraFiles` in Go's `exec.Cmd`. The child receives them at fd 3, 4, 5, etc. (after stdin/stdout/stderr).

This is how a TCP listener's FD can be passed from one process to another. Both processes hold a `dup` of the same kernel-level listener. Either can call `accept()` on it. Closing one process's `dup` does not affect the kernel listener until the last `dup` is closed.

This is the mechanism Fix 1 uses: supervisor binds, supervisor `dup`s into worker via `cmd.ExtraFiles[0]`, worker calls `net.FileListener` to wrap the inherited FD as a `net.Listener`, both supervisor and worker can in principle accept (in practice only worker accepts; supervisor just holds the FD to keep the kernel listener alive).

### launchd

macOS's system service manager. Started at boot, manages user-level and system-level long-running processes. The plist file at `~/Library/LaunchAgents/io.goodkind.clyde.daemon.plist` tells launchd to run `/Users/agoodkind/.local/bin/clyde daemon` at user login. launchd respawns the process if it crashes.

When `make deploy` runs `clyde daemon reload`, launchd is NOT involved. The reload is initiated from inside the daemon process itself via the supervisor's reload-control socket. launchd only steps in if the supervisor itself crashes (then launchd respawns the supervisor, which respawns the worker).

### Tack ticket states

The CLYDE project has these states:

- `Backlog`. Not yet planned. Probably won't be done soon.
- `Todo`. Planned but not started.
- `In Progress`. Currently being worked on.
- `Done`. Shipped (means: lives on `main`, not just on a feature branch).
- `Cancelled`. Decided not to do; won't be done.

Agents file new tickets in `Todo` by default. Agents do NOT move tickets between states without explicit user direction. The CLYDE-300 (live-runtime survival on reload via Setsid spawn + detach-only closers + filesystem manifest reattach for daemon-managed background subprocesses)/301/302/303/308 tickets currently sit in `Todo` (or wherever they were before; check Tack). The user will move them to `Cancelled` after CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) lands.

### MCP (Model Context Protocol)

A standard protocol for tools to connect to LLM contexts. Clyde uses MCP in two ways:

1. As a client. The Tack MCP server provides tools like `mcp__tack__tack_create_issue`. Clyde agents use these.
2. As a server. `clyde mcp` runs an MCP stdio server that exposes Clyde's session/MITM data to MCP clients.

### "Harmony"

The informal name for the May 2026 attempt to land the full CLYDE-308 supervisor-relay architecture on `main`. The branch was `golden/zdr-harmony` at `b27335c`. Deployed on 2026-05-09. Broke `clyde` launch within 3 seconds of the daemon starting. Was rolled back to `cb9fc7d` (pre-harmony) and the working snapshot saved as `backup/main-pre-harmony`. The name "harmony" is just the user's branch-naming convention; nothing magical about it. Whenever this doc says "harmony" it means "the broken CLYDE-308 deploy attempt that we rolled back from". When it says "pre-harmony", it means "main as it was before that deploy", which is `cb9fc7d`.

### "Phase 4" vs "Phase 4b"

CLYDE-308 was structured as four phases (Phase 1 = CLYDE-300, Phase 2 = CLYDE-301, Phase 3 = CLYDE-302, Phase 4 = CLYDE-303). Phase 4 was the gRPC stream relay through supervisor. The implementation was further broken into sub-phases 9a through 9h (see below) to keep individual commits reviewable. "Phase 4b" is informal shorthand for "the second wave of Phase 4 work" that landed on the `clyde-308-phase-4b` worktree. It wired stream replay for streaming RPCs other than `StreamLiveSession`. It was the work that uncovered the stringly-typed dispatch bug in Section 4. All Phase 4b worktrees are abandoned. Their commits are reverted out as part of CLYDE-309.

### Sub-phases 9a through 9h

The bottom-up implementation steps for the CLYDE-308 supervisor-relay architecture. Each was a separate commit/branch with its own worktree:

- **9a**: `internal/supproto` wire protocol (frame types: Hello, UnaryRequest, UnaryResponse, etc.) and the worker-side `WorkerClientConn` connection wrapper. Worktree `clyde-zdr-9a` at `97e9cb7`.
- **9b**: Supervisor-side `*grpc.Server` skeleton (`clydesup.GRPCServer`) that registers `UnimplementedClydeServiceServer` and a `UnaryForwardingProxy` stub.
- **9c**: Worker-side `WorkerRelayServer` listener on `daemon.worker.<gen>.sock`, accepting supproto frames.
- **9d**: `WorkerRegistry.SetActiveByHello` wiring so a freshly-spawned worker's Hello frame promotes it into the active slot. Cleared the bulk of dead-code findings.
- **9e**: Per-method unary dispatch table (30 entries, one per `ClydeServiceServer` unary method). Each entry knows how to unmarshal request bytes into the typed proto, call the typed handler on the local `*Server`, marshal the response.
- **9f**: Wire `Supervise()` to construct the `*grpc.Server` and register `GRPCServer` on it. The integration step that made the relay actually serve traffic.
- **9g**: Test migration. Update existing tests for the new supervisor-bound listener model.
- **9h**: Remove `drainReloadedLiveWorkers` (its responsibilities now live in the relay) and add an integration test for stream replay across worker swap.

The Phase 4b stream-relay work was a follow-on to 9d/9e/9f that wired stream replay for the 5 streaming RPCs other than `StreamLiveSession` (which 9d wired). The Phase 4b worktrees (`clyde-phase-4b`*) are abandoned in CLYDE-309.

### `clyde-zdr-restored` and friends ("ZDR")

ZDR = "zero disconnect reload". Branches and worktrees prefixed `zdr-` are CLYDE-308-era. `golden/zdr-restored` at `50b60f9` is the assembled golden branch with all CLYDE-308 work plus the Fix 1/2/3 + idiomatic refactor + TUI bundle + AGENTS.md docs + session tests + CLYDE-299 lumberjack fix. **Source for cherry-picks during the revert.** Do not delete until CLYDE-309 lands. After CLYDE-309 lands, `golden/zdr-restored` becomes a historical reference.

### `Fix 1`, `Fix 2`, `Fix 3`

These are the three architectural-correctness fixes built on top of CLYDE-308 to make it actually work. They are not Tack ticket numbers; they're informal names from this session.

- **Fix 1**: Supervisor binds the MITM TCP listener at `[::1]:48723`. Holds the FD for the entire daemon lifetime. Worker inherits via `cmd.ExtraFiles`. Eliminates the bind-gap on reload. Lives in worktree `clyde-fix-mitm-supervisor` at commit `736ceb5`. **This is the only Fix the user asked for.**
- **Fix 2**: Couple worker readiness to the Hello handshake. The worker is "ready" only when both the process pipe writes "ready\n" AND the Hello handshake registers the worker in the WorkerRegistry. Lives in `clyde-fix-worker-readiness` at `4843d98`. Was needed only because CLYDE-308 introduced the gap. Reverted out by CLYDE-309.
- **Fix 3**: Gen-monotonic guard on `WorkerRegistry.SetActiveByHello`. Rejects stale Hello frames where the supervisor's view of generation is newer than the Hello's. Lives in `clyde-fix-prehandover` at `ffbe2cf`. Same provenance and same fate as Fix 2.

### "Stringly-typed dispatch bug"

The specific bug that broke harmony's TUI dashboard streams. The supervisor (in `internal/clydesup/grpc_relay.go`) sent `StreamSubscribe` frames with `Method: "SubscribeRegistry"` (a bare string literal). The worker (in `internal/daemon/worker_relay_stream.go`) keyed its dispatch table by `clydev1.ClydeService_SubscribeRegistry_FullMethodName` which evaluates to `/clyde.v1.ClydeService/SubscribeRegistry`. The lookup `streamHandlers[sub.Method]` returned nothing because `"SubscribeRegistry" != "/clyde.v1.ClydeService/SubscribeRegistry"`. The worker returned an error frame: `worker relay: unknown streaming method "SubscribeRegistry"`. The supervisor relayed that to the gRPC client as `Internal`. The TUI dashboard (which subscribes to `SubscribeRegistry` for session-list updates and `SubscribeProviderStats` for stats) saw every subscription fail. The error was masked because:

1. The previous tests for these streaming RPCs used `fakeStreamWorker` which acks ANY method name regardless of dispatch table.
2. `StreamLiveSession` happened to use bare strings on both sides, so the bug didn't manifest there.
3. Go's type system can't help: `string` is `string`, no nominal typing for literals (untyped string constants implicitly convert to `type X string`).

The full story is in this doc's section 4 and the `feedback_fix_root_not_symptom.md` memory.

### "Wave 1" / "Wave 2" agents

Informal sequencing names from this session for parallel agent waves spawned to implement Phase 4b. Wave 1 = the worker dispatch + supervisor handler implementations (in parallel). Wave 2 = the test agents (in parallel). Wave 2A was registry+stats tests; 2B was transcript+compact tests; 2C was reload-survival tests. Wave 2A is what surfaced the stringly-typed bug.

### "Cruft tickets"

Tack tickets filed when an agent honestly scoped its work and left residual gaps that the user asked to be tracked rather than swept under the rug. The behavioral rule (section 11.3) requires that any deferred work be filed as cruft, not framed as "fine to keep". CLYDE-310 through CLYDE-318 are all cruft tickets in this sense.

### "OSS-21" through "OSS-25"

Tickets filed against the OSS project (Tack workspace `main`, project `OSS`) for go-makefile static analyzer rules that would have prevented the CLYDE-308 bug class. Each describes a detector for a specific bypass pattern (slog-boxing, synthetic marker calls, throwaway service registrations, no-op closers, silent error discards). The user's go-makefile agent owns implementation. Agents in this Clyde repo do not implement these.

---

## 20. The actual sequence of work for the next LLM

If you're actually executing the revert (not just reading this for context), here's the concrete sequence:

### Step 1: Ground yourself

```
git -C /Users/agoodkind/Sites/clyde-dev/clyde rev-parse --short HEAD
# Expected: cb9fc7d
```

If it's not `cb9fc7d`, stop and ask the user. Something has changed since this doc was written.

```
pgrep -afl 'clyde daemon'
# Expected: 1 supervisor + 1 worker, both running cb9fc7d binary.
```

If the daemon isn't running cb9fc7d, see the troubleshooting section at the end of this doc.

### Step 2: Run CLYDE-319 (Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected; investigation prerequisite for the revert) investigation first

This determines whether Fix 1 is even needed.

Spawn an Explore agent to read `internal/mitm/proxy.go` `Shutdown` and `internal/daemon/run.go::reloadDaemonBinary` on cb9fc7d and trace the listener-close sequence. Brief the agent thoroughly. Cite the questions in section 15 Investigation A.

If the agent reports "no bind-gap, FD inheritance is sufficient", then CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) is a no-op and you're done. Update the Tack tickets, write a verification test, deploy.

If the agent reports "bind-gap exists", proceed to Step 3.

### Step 3: Create the revert worktree

```
git -C /Users/agoodkind/Sites/clyde-dev/clyde worktree add -b revert/clyde-308 \
  /Users/agoodkind/Sites/clyde-dev/clyde-revert-308 cb9fc7d
```

### Step 4: Spawn the implementation agent

Brief an Opus general-purpose agent with:

- The full context of section 8 (the commit sequence).
- The Fix 1 reference function locations in `clyde-golden-restored`.
- The hard rules from section 11 (especially baseline-files-are-user-only, no em-dashes, git-requires-explicit-C).
- The verification gates per commit.

Have the agent commit one at a time, verifying gates between commits. Stop and report if any commit cannot pass gates.

### Step 5: Verify on `revert/clyde-308`

Once all commits land:

- `make build` (signed binary at `dist/clyde`)
- `make test`
- `make lint`
- `make staticcheck-extra`
- `make govulncheck`
- `make fmt && git diff --exit-code`

### Step 6: Deploy

Ask the user to wind down active LLM sessions. Then:

```
cd /Users/agoodkind/Sites/clyde-dev/clyde-revert-308
make deploy
```

This installs the binary, updates launchd, runs `clyde daemon reload`.

### Step 7: Live verification

Per section 8 verification gate:

- (a) MITM zero-bind-gap. Hold an HTTP stream open, reload mid-stream, assert no upstream "reconnecting" cascade.
- (b) gRPC clean retry on reload. Subscribe a TUI stream, reload, assert clean disconnect-and-retry.
- (c) Adapter and webapp listeners survive reload.

### Step 8: Land on main

If verification passes:

```
git -C /Users/agoodkind/Sites/clyde-dev/clyde merge --ff-only revert/clyde-308
```

Push:

```
git -C /Users/agoodkind/Sites/clyde-dev/clyde push origin main
```

### Step 9: Cleanup

Spawn a sonnet branch-deletion-sweep agent (per CLYDE-318 (Worktree + branch cleanup after the revert lands; ~50 abandoned worktrees and ~20 branches need pruning)). Keep the protected list: `main`, `fixup/*`, `golden/*`, `backup/*`, anything attached to a worktree, anything not an ancestor of `main`. Remove worktrees that are merged.

### Step 10: Update Tack

Move CLYDE-300 (live-runtime survival on reload via Setsid spawn + detach-only closers + filesystem manifest reattach for daemon-managed background subprocesses), 301, 302, 303, 308 to `Cancelled` (with the user's explicit OK). Move CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) to `Done`. Add a summary comment.

---

## 21. Troubleshooting (if state isn't what this doc says)

### If main isn't at cb9fc7d

Either someone has merged something since this doc was written (good, look at the new state) or there was a force-push (bad, ask the user). The backup ref `backup/main-pre-harmony` should always be at `cb9fc7d`. You can recover with:

```
git -C /Users/agoodkind/Sites/clyde-dev/clyde reset --hard backup/main-pre-harmony
```

Only do this if the user confirms.

### If the daemon is running a different binary

Check:

```
launchctl list | grep clyde
```

If it shows the daemon as `not running` or with an error code, run:

```
launchctl bootout gui/$UID/io.goodkind.clyde.daemon
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/io.goodkind.clyde.daemon.plist
```

Or use `make deploy` to reinstall and start fresh.

### If you find the worktree count is way off (much less than 90)

Someone ran a worktree cleanup. Check `git -C /Users/agoodkind/Sites/clyde-dev/clyde worktree list` and adapt. The important worktrees that should still exist:

- `clyde` (main checkout)
- `clyde-golden-restored` (cherry-pick reference; needed until CLYDE-309 (Revert the supervisor-relay architecture; keep Fix 1 MITM listener supervisor only; the urgent tracking ticket this whole doc is about) lands)
- `clyde-fix-mitm-supervisor` (Fix 1 reference)

If `clyde-golden-restored` has been removed, you can recreate it:

```
git -C /Users/agoodkind/Sites/clyde-dev/clyde worktree add /Users/agoodkind/Sites/clyde-dev/clyde-golden-restored golden/zdr-restored
```

### If you cannot find a Tack ticket referenced in this doc

Tack might have been pruned, or the ticket was renumbered. Use:

```
mcp__tack__tack_list_issues with workspace_reference="main", project_reference="CLYDE"
```

To see the current state. CLYDE-300 (live-runtime survival on reload via Setsid spawn + detach-only closers + filesystem manifest reattach for daemon-managed background subprocesses) through CLYDE-319 (Confirm cb9fc7d MITM listener actually has bind-gap or is already FD-inheritance-protected; investigation prerequisite for the revert) are all referenced in this doc and should all exist as of 2026-05-09 18:35 PT.

---

## 22. Last words

You will be tempted to add more architecture, more types, more layers. Resist. The user's stated requirement is small. The path to satisfying it is small. The path to NOT satisfying it has been very, very large.

If you find yourself thinking "but the architecturally correct thing would be...", stop. Read section 4 again. Read section 11.4. The architecturally correct thing is often not what the user asked for.

If you find yourself thinking "this would be nice to also fix while we're here", stop. File a separate Tack ticket. Do not bundle.

If you find yourself thinking "let me ask the user one more clarifying question", check whether you've already asked something similar this turn. If yes, just execute on your best understanding and report.

The user trusts you to read this doc and act on it. Don't betray that trust by re-doing the same mistakes.