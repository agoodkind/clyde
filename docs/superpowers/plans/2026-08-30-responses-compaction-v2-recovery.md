# Native Responses compaction v2 recovery implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve a bounded lower transcript across native v2 compaction by forcing it into post-compaction requests, then persisting it through the first regular final assistant response.

**Architecture:** Turn N reuses the existing Codex normalizer, complete-turn selector, tool-pair validator, generic transcript renderer, tags, and raw request helpers. Its encrypted response stays byte-identical. A bounded process-local registry carries the rendered transcript through later tool rounds. Clyde injects one synthetic assistant history item into matching upstream requests and reuses the regular response transformer to append the transcript once, after which Codex resends it naturally.

**Tech Stack:** Go, OpenAI Responses Server Sent Events, zstd, `httptest`, race tests, and isolated Codex 0.151.0 validation.

**Spec:** [Native Responses compaction v2](../specs/2026-08-30-responses-compaction-v2-design.md)

## Global Constraints

- Reuse `NormalizeResponseInputItems`, `rawCompactionUnits`, `rawCompactionPairIntervals`, `rawCompactionPairsAreComplete`, `renderRawResponsesCompactionItems`, `wrappedRawCompactionTranscript`, and `RawResponsesCompactionTransformer`.
- Do not add another transcript parser, transcript model, renderer, or tag format.
- Keep v1 `responses` behavior unchanged.
- Keep the v2 encrypted compaction response byte-identical.
- Key pending recovery by exact `session_id` plus SHA256 of `encrypted_content`.
- Keep at most 32 pending entries. Expire entries after two hours. Evict the oldest entry at capacity.
- Keep pending state process-local as an intentional staged-recovery boundary,
  not durable persistence. Restart, expiry, and capacity eviction after
  trimming fail open to native v2 behavior.
- Reuse existing reorient controls. Add no configuration keys.
- Atomically reserve one pending entry for each regular `final_answer`. Release
  that lease on any failed transformation or client delivery.
- Clear state only after one successful regular `final_answer` append reaches
  the client. This does not claim that Codex has durably persisted the result.
- Do not rotate, replace, write, or print token values.
- Do not deploy, restart, or replace the production daemon.

---

### Task 1: Repair shared primitive blockers

**Files:**

- Modify: `internal/mitm/capture/redact.go`
- Modify: `internal/mitm/capture/redact_test.go`
- Modify: `internal/adapter/codex/raw_compaction.go`
- Modify: `internal/adapter/codex/raw_compaction_sse.go`
- Modify: `internal/adapter/codex/raw_compaction_test.go`

**Interfaces:**

- Consumes: the final whole-branch adversarial report.
- Produces: safe shared parsing and streaming primitives.

- [ ] **Step 1: Add hostile regression tests**

Cover sensitive scalar values inside valid structured bodies, truncated completed frames, 300,000 comment-only frames under a watchdog, 10,001 small transcript items under a watchdog, and duplicate target JSON fields.

- [ ] **Step 2: Run focused tests to verify RED**

Run `go test ./internal/mitm/capture ./internal/adapter/codex -run 'TestRedact|TestRawResponsesCompaction' -count=1`.

- [ ] **Step 3: Fix the complete classes**

Scan the final redacted structure, fail open on completed-frame read errors, use amortized frame append, select the transcript suffix in one bounded pass, and reject duplicate target fields.

- [ ] **Step 4: Run the focused tests**

Expected: PASS within every watchdog.

- [ ] **Step 5: Commit Task 1**

Commit with subject `Harden native Responses compaction primitives` and the Codex co-author trailer.

---

### Task 2: Build the v2 tail plan from existing primitives

**Files:**

- Modify: `internal/adapter/codex/raw_compaction.go`
- Modify: `internal/adapter/codex/raw_compaction_v2.go`
- Modify: `internal/adapter/codex/raw_compaction_v2_test.go`

**Interfaces:**

- Consumes: `ParseRawResponsesCompactionV2` and the shared unit, pair, selection, raw-array, and rendering helpers.
- Produces `PlanRawResponsesCompactionV2(request, settings) (RawResponsesCompactionV2Plan, bool)` with transformed request, rendered transcript, and session ID.

- [ ] **Step 1: Write failing plan tests**

Prove exact setup and trigger bytes, complete upper and lower coverage, whole turns, complete tool pairs, and shared-renderer output.

- [ ] **Step 2: Run tests to verify RED**

Run `go test ./internal/adapter/codex -run '^TestPlanRawResponsesCompactionV2' -count=1`.

- [ ] **Step 3: Implement the thin v2 planner**

Do not call the v1 planner because it requires a synthesized user prompt. Reuse its lower-level primitives and replace only selected transcript items.

- [ ] **Step 4: Run v1 and v2 planner tests**

Run `go test ./internal/adapter/codex -run 'TestPlanRawResponsesCompaction|TestPlanRawResponsesCompactionV2' -count=1`.

- [ ] **Step 5: Commit Task 2**

Commit with subject `Plan Responses compaction v2 transcript tails` and the Codex co-author trailer.

---

### Task 3: Arm bounded recovery state from turn N

**Files:**

- Create: `internal/adapter/codex/raw_compaction_v2_state.go`
- Create: `internal/adapter/codex/raw_compaction_v2_state_test.go`
- Create: `internal/adapter/codex/raw_compaction_v2_observer.go`
- Create: `internal/adapter/codex/raw_compaction_v2_observer_test.go`
- Modify: `internal/adapter/server.go`
- Modify: `internal/adapter/server_responses.go`
- Modify: `internal/adapter/server_responses_zstd.go`

**Interfaces:**

- Consumes: `capture.CappedBuffer`, `TurnMetadata.SessionID`, and one successful v2 response.
- Produces a mutex-protected registry with `Arm`, `Match`, `Reserve`,
  `Release`, and generation-fenced `Complete` operations keyed by session and
  compaction digest.

- [ ] **Step 1: Write failing registry tests**

Cover exact matching, concurrent sessions, same-thread lease exclusion,
generation-fenced stale completion, capacity 32, two-hour expiry with an
injected clock, oldest eviction, completion, duplicate arm, and failure cleanup.

- [ ] **Step 2: Write failing response observer tests**

Cover JSON, Server Sent Events, zstd, malformed bodies, duplicate compaction items, unsuccessful responses, and oversized observer copies. Require client-visible bytes and headers to remain exact.

- [ ] **Step 3: Run tests to verify RED**

Run `go test ./internal/adapter/codex -run 'TestRawResponsesCompactionV2Registry|TestRawResponsesCompactionV2Observer' -count=1`.

- [ ] **Step 4: Implement registry and observer**

Follow the mutex and expiry pattern from `WebsocketSessionCache`. Use
`capture.CappedBuffer`. Store the transcript, IDs, digest, timestamps, lease
generation, and status. Never store encrypted content or credentials. Arm only
after Clyde has copied the byte-identical v2 response to the client. The
process-local staging window is intentional: restart, expiry, and eviction fail
open rather than persisting plaintext recovery state.

- [ ] **Step 5: Run focused and race tests**

Run the focused tests normally and with `-race`.

- [ ] **Step 6: Commit Task 3**

Commit the new registry and observer plus `internal/adapter/server.go`,
`internal/adapter/server_responses.go`, and
`internal/adapter/server_responses_zstd.go` with subject
`Track pending Responses compaction v2 recovery` and the Codex co-author
trailer.

---

### Task 4: Force pending transcript into post-compaction requests

**Files:**

- Create: `internal/adapter/codex/raw_compaction_v2_inject.go`
- Create: `internal/adapter/codex/raw_compaction_v2_inject_test.go`
- Modify: `internal/adapter/codex/raw_compaction.go`
- Modify: `internal/adapter/server_responses.go`
- Modify: `internal/adapter/server_responses_zstd.go`

**Interfaces:**

- Consumes: one armed registry entry and a regular native request carrying its encrypted compaction item.
- Produces `InjectRawResponsesCompactionV2Recovery(request, registry) (request, transformer, changed)`.

- [ ] **Step 1: Write failing request injection tests**

Require one synthetic assistant item immediately after the matching encrypted
item. Preserve all original items and unrelated fields. Require an atomic lease
so overlapping same-thread requests cannot both inject. Reject wrong sessions,
digests, malformed requests, duplicate compaction items, expired state, and
existing transcript tags.

- [ ] **Step 2: Run tests to verify RED**

Run `go test ./internal/adapter/codex ./internal/adapter -run 'TestInjectRawResponsesCompactionV2Recovery|TestNativeCodexResponsesCompactionV2RecoveryRequest' -count=1`.

- [ ] **Step 3: Implement request injection**

Normalize only to locate the encrypted item. Use shared raw JSON helpers. Insert an assistant `output_text` item containing `wrappedRawCompactionTranscript(transcript)`. Re-encode zstd through the existing request path. Return the existing response transformer configured for `final_answer` only.

- [ ] **Step 4: Run the focused tests**

Expected: PASS.

- [ ] **Step 5: Commit Task 4**

Commit with subject `Inject pending v2 transcript into native requests` and the Codex co-author trailer.

---

### Task 5: Persist recovery through the first regular final response

**Files:**

- Modify: `internal/adapter/codex/raw_compaction.go`
- Modify: `internal/adapter/codex/raw_compaction_sse.go`
- Modify: `internal/adapter/codex/raw_compaction_test.go`
- Modify: `internal/adapter/server_responses_native_test.go`

**Interfaces:**

- Consumes: the shared response transformer and one armed registry entry.
- Produces: a final-answer target policy and one-shot completion signal.

- [ ] **Step 1: Write failing lifecycle tests**

Cover streaming and non-streaming final answers, commentary, tool-call-only
responses, failures, malformed bodies, interruptions, duplicate tags,
same-thread overlap, and concurrent threads. For streaming responses, assert
that every original event remains in order and its original sequence or index
is unchanged, while the transcript appears exactly once. Keep state armed until
the first regular final answer carries the transcript and Clyde delivers it to
the client.

- [ ] **Step 2: Run tests to verify RED**

Run `go test ./internal/adapter/codex ./internal/adapter -run 'TestRawResponsesCompactionV2RecoveryResponse|TestNativeCodexResponsesCompactionV2RecoveryLifecycle' -count=1`.

- [ ] **Step 3: Extend the shared transformer**

Keep v1 selection unchanged. Require `phase: final_answer` for v2 recovery.
Invoke the completion signal only after the transformed bytes have reached the
client. Release the lease when transformation or delivery fails. Client delivery
is the observable boundary, not proof that Codex has durably persisted history.

- [ ] **Step 4: Prove natural N+2 resend**

Drive N compaction, N+1 request injection, N+1 final response append and client
delivery, and N+2. Require N+2 to contain exactly one tagged transcript without
forced mutation.

- [ ] **Step 5: Run focused and race tests**

Run the lifecycle tests normally and with `-race`.

- [ ] **Step 6: Commit Task 5**

Commit with subject `Persist v2 recovery through regular Responses output` and the Codex co-author trailer.

---

### Task 6: Verify and document v2 recovery

**Files:**

- Modify: `docs/adapter/compatibility.md`
- Modify: `docs/reorient/overview.md`
- Modify: `docs/adapter/testing.md`
- Test: `internal/adapter/server_responses_native_test.go`

**Interfaces:**

- Consumes: Tasks 1 through 5.
- Produces: reviewed code, one durable behavior contract, and isolated live evidence.

- [ ] **Step 1: Update behavioral documentation**

Document the N to N+1 contract once. Keep generic, v1, and v2 behavior separate. State the process-local fail-open window and built-in `openai` plus `openai_base_url` requirement.

- [ ] **Step 2: Run repository gates**

Run `make check`, full tests, race tests, and shuffled tests.

- [ ] **Step 3: Run isolated current-Codex validation**

Use Codex 0.151.0 with built-in `openai`, `openai_base_url` pointed at throwaway Clyde, v2 enabled, a 2,000-token context window, and a 1,500-line shell result. Require v2, byte-identical encrypted output, N+1 request injection, one regular response append, natural N+2 resend, zero credential matches, unchanged credential metadata, unchanged production processes, and closed ports.

- [ ] **Step 4: Run strongest-model adversarial review**

Attack correlation, duplicates, expiry, concurrency, phases, oversized transcripts, compression, truncation, parser differentials, and fail-open behavior. Record the review row.

- [ ] **Step 5: Reconcile Tack**

Mark CLYDE-704 Done after the mechanism passes review. Mark CLYDE-705 Done after live validation. Keep CLYDE-694 In Progress until both children are Done.
