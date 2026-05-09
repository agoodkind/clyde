# CLYDE-170: Reintroduce Auto-Rename Safely

## 1. Summary

CLYDE-170 reintroduces automatic session naming. The previous attempt was deleted because it conflated three distinct ideas. Those ideas were a human-readable label, a directory identifier, and a provider session id. This plan separates them. It restores LLM-driven naming as a daemon-owned, observable, automatic-apply path that funnels every rename through the existing `RenameSession` RPC mutation point.

Decisions locked by the user:
- Auto-rename applies the new name automatically. There is no in-product accept or reject affordance. The existing manual `Rename` entry in the options modal is the opt-out: a name set by the user is permanently locked from future auto-rename.
- Trigger fires when a cold session reaches three user messages. Cooldown is 30 minutes.
- Naming input is exactly those first three user messages, redacted before the source sees them.
- The adapter call uses a distinct `[autoname].provider` route key. The default value points at the same model used for summaries so day-one behavior matches.
- `[autoname].enabled` defaults to `true`. The feature ships on for new users.
- The TUI surface does not change. No badge. No new modal entries. The existing `KIND_SESSION_RENAMED` event already updates the dashboard row when the rename lands.
- The TUI never waits on the auto path. Trigger detection is sub-millisecond. The LLM call runs on a daemon worker goroutine, off any render loop. Section 6 enumerates the latency budget and goroutine boundaries.

The rollout is five discrete PRs. PR1 is data model and proto. PR2 hardens the rename funnel with the live-wrapper-lease gate. PR3 lands the kill-switch config block. PR4 lands the suggestion engine, the validation, and the apply path behind a transcript-only source. PR5 swaps in the LLM source.

## 2. Why the previous attempt was unsafe

The history is concentrated in five commits on the `clotilde` and `clyde` lineage. Read together they show four concrete failure modes.

Failure 1: rename of the on-disk directory. Commit `ea3dca1` ("Remove DisplayName field, add store.Rename") replaced the safe `DisplayName` metadata field with `store.Rename(oldName, newName)`. `Rename` calls `os.Rename(oldDir, newDir)` and rewrites `sess.Metadata.Name`. That path is incompatible with live wrappers. The daemon keys its `wrapperSession` map by `wrapper_id`, but `AcquireSession` writes a per-session settings file and the wrapper opens artifacts under the session directory. Renaming the directory while a wrapper holds it yields a stale path that the wrapper cannot reopen. The old code performed the rename without coordinating with the live-session leases held in `internal/daemon/live_sessions.go`. That made every "auto-name --all" sweep a race against any live wrapper.

Failure 2: orphan SDK transcripts. Commit `86f1daa` ("Add prune-autoname for orphan sdk-cli transcripts") explicitly documents the symptom. Each `auto_name.go` invocation called `claude -p` as a subprocess. The subprocess wrote a fresh transcript file under `~/.claude/projects/<encoded>/` for every naming call. Those files polluted the native `claude --resume` picker and were invisible to the Clyde dashboard. The fix shipped a janitor (`internal/prune/autoname.go`) instead of stopping the leak. Reintroducing auto-name without fixing the leak at the source ships the same garbage.

Failure 3: parent reference mutation. The same `store.Rename` walks every session and mutates `Metadata.ParentSession` on children. The walk is best-effort with a swallowed error (`return nil // non-fatal`). A failure mid-walk leaves orphan children pointing at a name that no longer exists. Fork lookup becomes inconsistent. There is no event surface that announces partial completion.

Failure 4: implicit `--force` collision policy. The original `auto-name --all --force` regenerated names in place. There was no rule that protected user-typed names. There was no per-session lock. The user could not opt a single session out without leaving the feature entirely.

Failure 5 (latent): unsafe LLM input. `generateDisplayName` sliced 20 transcript messages at up to 300 characters per message into a prompt and shelled out to `claude -p`. Raw user prompts and assistant output went to a child process. Errors from that child surfaced through `cmd.Output()` without redaction. The `AGENTS.md` content-safety rule was never honored.

The plan below treats those five failures as five independent invariants.

## 3. Invariants this design must preserve

Invariant A. Provider session id is never affected by display rename. The field `session.Metadata.ProviderSessionID` and its history in `Metadata.PreviousSessionIDs` plus `ProviderState.Previous` flow through `Identity()` unchanged.

Invariant B. The directory identifier (`Session.Name`) is never mutated by an automated path. Auto-rename writes a separate human-readable label. The dashboard renders the label. Resume by name, fork, compact, and adopt continue to use `Session.Name` as the stable directory key.

Invariant C. The user always wins. If the user typed a name in the rename modal, that name is locked. The only path that can clear the lock is an explicit user action.

Invariant D. The rename mutation point is `Server.RenameSession` in `internal/daemon/server.go`. Every code path that intends to mutate `Session.Name` must call that RPC. No package below the daemon may call `store.Rename` directly. The existing call site at `cmd/root.go:320` already routes through `daemon.RenameSessionViaDaemonOutcome`.

Invariant E. LLM input is bounded summaries, never raw transcript content. The prompt template asserts a length cap, a charset whitelist, and a non-PII instruction. Logs carry `prior_name`, `new_name`, and `source`. Logs do not carry transcript bytes.

Invariant F. Daemon reload contract holds. AGENTS.md cite: "Active session runtime dirs must survive reload drain so wrappers and remote-control sockets can reacquire against the child." A reload mid-suggestion cancels the suggestion. The applied rename, if any, is observable through `KIND_SESSION_RENAMED` so a reconnecting subscriber sees the post-rename state.

## 4. Policy: when Clyde may apply, when it must defer

A session has an `auto_name_state` that is one of `untouched`, `applied`, or `user_locked`. A session has an `auto_name_source` that is one of `default`, `transcript`, `llm`, or `user`. The combination drives policy.

Trigger 1. Cold session with default-shaped name reaches three user messages. The default-shaped predicate matches the existing scan-defaults heuristic in `internal/session/scan_defaults.go`. The user-message-count source is the daemon's existing message counter. The candidate name is generated from exactly those first three user messages: they are the only input the worker passes downstream (after redaction). The session never enters the auto path before the third message and the worker never reads beyond message three for the initial naming pass.

Trigger 2. Idle gate. Apply fires only when no live-session lease is held for the session. The gate consults the existing live-session map. If a wrapper is mid-conversation, the rename is queued and re-evaluated when the lease releases.

Trigger 3. Cooldown. A session's `last_auto_name_at` must be older than 30 minutes. Cooldown applies even after a successful rename, so a rapid back-and-forth conversation does not re-rename the same session three times in a minute.

Apply behavior. The default and only mode is automatic apply. The worker constructs the candidate name, validates it (Section 7), and calls `Server.RenameSession` directly. There is no human-in-the-loop accept step. The user opts out per session by issuing a manual rename through the existing options modal entry, which sets `auto_name_source = user` and locks the session against future auto-rename.

User-owned predicate. A name is user-owned when `auto_name_source = user` or `auto_name_state = user_locked`, or the name does not match the default-shaped predicate. A user-owned name is never overwritten by the auto path. The existing `Server.RenameSession` flow records `auto_name_source = user` whenever the call originates from the rename modal callback.

Defer rules. Apply defers when a wrapper holds a live-session lease. Apply defers when daemon reload is in progress. Apply defers when the global rate limit (default 6 calls per hour, configurable) is exhausted. A deferred apply is re-evaluated when the gate clears, not retried in a tight loop.

Global kill switch. `[autoname].enabled = false` disables the worker entirely. The default value of this knob is the safety question the user must answer; see Section 11.

## 5. Data model and proto reservations

Go-side `Metadata` gains three typed fields. None replace existing fields. All are additive. The previous draft included a `DisplayNameLocked` flag for the dropped accept-or-reject UI; with apply mode locked in, the lock signal collapses into `AutoNameSource = user`, so a separate boolean is unnecessary.

```go
AutoNameState   AutoNameState  `json:"autoNameState,omitempty"`
AutoNameSource  AutoNameSource `json:"autoNameSource,omitempty"`
LastAutoNameAt  time.Time      `json:"lastAutoNameAt,omitempty"`
```

`AutoNameState` and `AutoNameSource` are typed string enum aliases declared next to `ProviderID` in `internal/session/`. Type-hygiene rule: domain payloads use named typed enums.

`DisplayTitle` already exists. The new system writes to `DisplayTitle` for the suggested human-readable label. This avoids re-litigating the deleted `DisplayName` field. `Session.Name` continues to be the directory identifier.

Proto reservations on `clyde.v1.SessionSummary` (file `api/clyde/v1/daemon/session.proto`). Last used field number is 30 (`runtime`). The new fields take 31 through 33.

```proto
string auto_name_state      = 31;
string auto_name_source     = 32;
int64  last_auto_name_nanos = 33;
```

No new event kind is required. The existing `KIND_SESSION_RENAMED` event already announces the post-rename state, and the new `auto_name_*` fields ride on the `SessionSummary` payload that the rename event carries. Subscribers see the source attribution without a separate event type.

No new RPCs. The auto-rename worker mutates state through the existing `Server.RenameSession` RPC. A new RPC for accept or reject is unnecessary because there is no human accept step.

Configuration lives under `[autoname]` in `clyde.toml`. The block is a typed Go struct in `internal/config/`. The `Mode` field from the previous draft is removed; apply is the only behavior.

```go
type AutoNameConfig struct {
    Enabled         bool          `toml:"enabled"`
    Provider        string        `toml:"provider"`            // adapter route key
    MaxCallsPerHour int           `toml:"max_calls_per_hour"`
    Cooldown        time.Duration `toml:"cooldown"`
    MinUserMessages int           `toml:"min_user_messages"`
    Redact          RedactPolicy  `toml:"redact"`
}
```

Default values: `MaxCallsPerHour = 6`, `Cooldown = 30 * time.Minute`, `MinUserMessages = 3`. The default value of `Enabled` is the open question in Section 11. The default value of `Provider` is the same adapter route key the summary subsystem uses today, so day-one behavior matches the model the operator already trusts.

## 6. Daemon-side architecture and worker placement

The worker lives in a new package `internal/sessionrename/`. The package owns the suggestion engine, the rate limiter, the single-flight registry, and the safe LLM call. It does not own the actual rename. The mutation funnels through `Server.RenameSession` per Invariant D.

Inputs. The worker subscribes to the existing registry event stream (`SubscribeRegistry`) and to a new internal idle ticker. AGENTS.md cite: "Non-UI work triggered from the TUI must run through daemon-owned or daemon-backed async paths." All of this is daemon-owned.

Concurrency. A `singleflight.Group` keyed by `Session.Name` guarantees one suggestion per session in flight. A semaphore caps global concurrency at 2. The configured rate limiter is a token bucket scoped to the daemon process.

Adapter call. The worker uses the daemon's existing adapter to resolve the configured `[autoname.provider]` route. The original implementation shelled out to `claude -p`, leaving SDK-CLI transcript orphans. The new implementation calls the daemon's adapter directly. No subprocess. No transcript leak. PR6 deletes `internal/prune/autoname.go` once the call path is proven.

Failure modes. Adapter probe failure surfaces a structured slog event and clears the single-flight slot. Model unavailable returns the same. Empty transcript returns a `transcript-source-empty` decision and the worker emits no name. Daemon reload mid-call uses `context.Context` cancellation so the partial result is dropped.

Mutation. The worker calls `Server.RenameSession` from inside the daemon as soon as a candidate clears validation. The same `KIND_SESSION_RENAMED` event subscribers already see propagates the change. The TUI rename event handler at `internal/ui/app.go:1846` already handles the rename case correctly. No new RPC is required.

Live-session gate. Before the worker calls `Server.RenameSession`, it consults the live-session lease map. If a wrapper holds the lease, the candidate is parked and the worker subscribes to the lease-release event. When the lease releases, the worker re-runs validation against the current name (the user may have renamed in the meantime) and applies if and only if the gates still allow it. This closes Failure 1 from history without surfacing a transient error to a user who never asked for the rename.

### Latency budget and non-blocking guarantees

The TUI must never block on any auto-rename step. The chat must never wait. The dominant cost is the LLM call; everything else stays sub-second. The decomposition below names every async boundary.

The trigger-evaluation goroutine. The registry-event subscriber inspects each `SESSION_UPDATED` event in O(1) cache lookups. It must not call the adapter, must not read the transcript, and must not acquire the file store lock for more than a single map read. If the trigger condition holds, the subscriber enqueues a job onto a buffered channel and returns immediately. Budget: 1 ms per event.

The worker pool. A small fixed pool of goroutines (default 2, capped by the existing semaphore at `internal/daemon/server.go:96`) drains the job channel. Each worker reads three messages from the transcript, redacts them, calls the adapter, validates the response, and invokes `Server.RenameSession`. The worker's context derives from the daemon lifetime context so a daemon reload cancels in-flight work. Budget: dominated by the adapter call (typical 2 to 30 seconds, ceiling enforced by the adapter route's existing timeout).

The transcript read. Bounded to the first three user messages plus a 240-character cap per message. The read uses the existing `inspectAllMessages` helper with a `limit` argument so we never scan the full file. Budget: 10 ms.

The adapter call. Runs on the worker goroutine, never on the trigger goroutine. Honors the configured adapter route timeout. Budget: ceiling 60 seconds, no soft target.

`Server.RenameSession`. Already exists. Performs `os.Rename` of the session directory plus a bounded walk of child sessions to update `Metadata.ParentSession`. The walk is currently best-effort with swallowed errors and the plan replaces that with a transactional update that reports a hard error on partial failure. The walk is bounded by the cardinality of children, which is small. Budget: 100 ms typical, 1 second upper bound. The walk runs on the calling goroutine. Callers must be the auto worker or the TUI rename callback, both off the TUI render loop.

The TUI side. The TUI never initiates auto-rename. The TUI receives `KIND_SESSION_RENAMED` events through the existing subscription and applies them in `applySessionEvent`. That handler is already sub-millisecond because it only mutates in-memory maps. No new TUI work is required, and no draw path is touched.

Backpressure. The job channel is buffered (size 16). If the buffer fills (a burst of new sessions exceeding the worker pool), the trigger subscriber drops the oldest pending job and logs `daemon.autoname.queue_full`. Dropped jobs are not retried automatically. The cooldown gate ensures the same session re-enters the queue at the next opportunity, not in a tight loop.

Reload safety. A daemon reload cancels every in-flight worker context. Parked candidates (live-lease gate) are kept in metadata, not in memory, so the reload child re-evaluates them on first event delivery after handoff.

## 7. Conflict handling rules

Rule 1. A suggested name that collides with an existing `Session.Name` is rejected at validation time inside the worker. The worker emits a `suggestion-collision` event and tries again at the next trigger. The session retains the prior `auto_name_state`.

Rule 2. A suggested name that collides with another suggestion in flight is serialized through the single-flight key on the candidate name. The first writer wins. The second writer rejects.

Rule 3. A suggestion that targets a user-owned name on another session is rejected with reason `target_user_owned`. The worker does not propose the same candidate again within the cooldown.

Rule 4. A user-driven rename arriving while a suggestion is in flight cancels the suggestion. The user rename always wins. The cancellation lives in the daemon worker, not in the TUI.

Rule 5. Suffix increment is the duplicate-name resolution rule. If a candidate `acme-bgp-cutover` collides, the worker tries `acme-bgp-cutover-2` once. A second collision is a hard reject.

## 8. Observability, opt-out, content safety

Kill switch. `[autoname].enabled` is the global toggle. The package exposes a `Disabled() bool` that the daemon consults at start. The default value of this knob is the only open question on the safety surface.

Per-session opt-out. There is no dedicated lock affordance. The existing manual `Rename` entry in the options modal is the opt-out. When the user invokes that entry, `Server.RenameSession` records `auto_name_source = user`, and the worker's user-owned predicate excludes the session from any future apply pass. This is invariant C and it is enforced at the worker, not at the UI.

Structured logs. Every suggestion and application emits one `slog.Info` event with these fields: `component=daemon`, `subcomponent=autoname`, `trace_id`, `span_id`, `session`, `provider_session_id`, `source`, `model`, `duration_ms`, `prior_name`, `suggested_name`, `applied`.

Status surface. `SessionSummary.auto_name_state` carries the worker's verdict. The TUI renders a single-character badge. There is no polling.

Content safety. The worker reads exactly the first three user messages from the transcript. Each message is trimmed to 240 characters and runs through a redaction pass that strips numbers longer than 6 digits, email-shaped substrings, file paths starting with `/`, and obvious key prefixes. The three redacted messages are concatenated with a separator and passed to the configured source. The transcript-only source (PR4) extracts the most distinctive noun phrase via simple heuristics. The LLM source (PR5) sends the concatenated text plus a prompt template that explicitly asks for a non-PII kebab-case session label and bans direct quotes from the input. Output validation enforces a kebab-case regex (`^[a-z][a-z0-9-]{1,48}[a-z0-9]$`), rejects names that match the provider session id pattern (UUIDv4), rejects names that look like file paths, and rejects names that match an existing user-typed name. The worker never reads beyond message three for the initial naming pass; later renames on the same session are not in scope for this milestone.

## 9. PR-by-PR rollout plan

PR1. Data model and proto reservations only. Adds `AutoNameState`, `AutoNameSource`, and `LastAutoNameAt` to `Metadata`. Adds the three new fields on `SessionSummary`. Runs `buf generate` once. No behavior changes. No new RPCs.

PR2. Daemon rename funnel hardening. Adds the live-session lease check at the entry of `Server.RenameSession`. Adds a per-mutation slog event with the full field set including `source`, `prior_name`, `new_name`. Refactors the existing `cmd/root.go` rename callback so user-driven renames record `auto_name_source = user`. No auto behavior yet.

PR3. Config plumbing. Adds the `[autoname]` config block as a typed struct. Daemon parses it. No worker yet. Default `Enabled` value is set per the user's decision in Section 11.

PR4. Apply engine, transcript-only source. Lands `internal/sessionrename/` with the worker, the rate limiter, the cooldown clock, the trigger gates, the validation, the live-lease parking. The candidate name is the trimmed first user message (no LLM). The worker calls `Server.RenameSession` directly. Removes `internal/prune/autoname.go` and its CLI command because the new path produces no SDK-CLI transcript orphans.

PR5. LLM-backed candidate source behind the existing config knob. Wires the adapter call to the configured `[autoname].provider` route. Adds the redaction pass and the prompt template. Adds the kebab-case validation regex. Default value of the provider knob points at the same model the summary subsystem already uses.

## 10. Test strategy

The table below tells the implementer which historical failure each test closes. Existing tests stay where they are.

| Test file | Action | Closes |
| --- | --- | --- |
| `internal/session/store_test.go` | Add cases for `Rename` rejecting a target that matches the provider session id pattern | Failure 5 latent reuse |
| `internal/session/store_test.go` | Add case asserting that `Rename` with a child whose `ParentSession` matches old name updates the child atomically and reports a hard error on partial failure (no swallowed error) | Failure 3 |
| `internal/session/session_test.go` | Add round-trip serialization for the four new metadata fields | PR1 |
| `internal/sessionrename/worker_test.go` | New file. Cover trigger gates, cooldown, rate limit, single-flight, live-session deferral, daemon reload cancellation | Failure 1, Failure 4 |
| `internal/sessionrename/redact_test.go` | New file. Cover redaction of digits, emails, paths, key prefixes | Failure 5 latent |
| `internal/sessionrename/validate_test.go` | New file. Cover kebab regex, UUID rejection, path rejection, existing-name collision, suffix increment | Failure 4, Rule 5 |
| `internal/daemon/server_test.go` | Add case asserting `Server.RenameSession` returns `FAILED_PRECONDITION` when a wrapper lease is held | Failure 1 |
| `internal/daemon/live_sessions_test.go` | Add case asserting that a live wrapper survives a queued auto-rename until lease release | Failure 1 |
| `internal/daemon/server_test.go` | Add adoption test that asserts `KIND_SESSION_RENAMED` carries `old_name` and the new `auto_name_*` fields | PR1 |
| `cmd/clyde/...` (rename smoke) | Add an end-to-end test for `daemon.RenameSessionViaDaemonOutcome` against a session with the new fields | PR2 |
| `internal/ui/app_ux_test.go` | Add a case asserting that a manual rename invocation records `auto_name_source = user` on the session metadata so the auto worker excludes it forever after | PR2 |
| `internal/prune/autoname_test.go` | Delete with the package in PR4 | PR4 |

Regression coverage for resume by name, fork, compact, and adopt is already exercised by `internal/session/scan_test.go`, `internal/session/resolve_tier4_test.go`, and `cmd/clyde` smoke tests. No changes are required to those files because the directory identifier never changes for the auto path.

## 11. Open questions for the user

Question 1. RESOLVED. `[autoname].provider` is a distinct adapter route key. Default value points at the same model the summary subsystem uses today.

Question 2. RESOLVED. Trigger fires after three user messages with a 30-minute cooldown. Default values match.

Question 3. RESOLVED. No new UI surface. The existing manual `Rename` entry in the options modal is the per-session opt-out.

Question 4. RESOLVED. `[autoname].enabled` defaults to `true`. The system just works for new users. The safety guards (live-lease gate, user-owned protection, validation, redaction, rate limit) close the prior incident. PR2 and PR4 must land without scope drift on any guard for this default to ship safely.

All four open questions are now resolved.
