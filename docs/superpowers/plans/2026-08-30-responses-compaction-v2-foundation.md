# Native Responses compaction v2 foundation implementation plan

The completed foundation continues in [Native Responses compaction v2 recovery](2026-08-30-responses-compaction-v2-recovery.md).

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `responses_compaction_v2` a separate, typed, byte-preserving protocol path.

**Architecture:** A protocol classifier routes `responses` to the existing text-summary handler and `responses_compaction_v2` to byte-preserving native forwarding. Recovery requires a separate reviewed design and new persistence evidence.

**Tech Stack:** Go, OpenAI Responses Server Sent Events, zstd, SQLite capture fixtures, `httptest`, and real Codex CLI sandbox validation.

**Spec:** [Native Responses compaction v2](../specs/2026-08-30-responses-compaction-v2-design.md)

## Global Constraints

- Keep `responses` text-summary compaction behavior unchanged.
- Keep encrypted v2 compaction items byte-identical.
- Keep v2 request and response bytes unchanged.
- Preserve unknown protocol implementations through native pass-through.
- Reuse existing reorient controls. Add no configuration keys.
- Prioritize incremental delivery for valid compressed streams. A late compression failure terminates the stream after decoded bytes were delivered; it does not attempt replay.
- Do not rotate, replace, write, or print token values.
- Do not deploy, restart, or replace the production daemon.

---

### Task 1: Add native compaction protocol classification

**Files:**

- Create: `internal/adapter/codex/raw_compaction_protocol.go`
- Create: `internal/adapter/codex/raw_compaction_protocol_test.go`
- Modify: `internal/adapter/codex/raw_compaction.go`

**Interfaces:**

- Consumes: `RawResponsesRequest.Header` and `CodexTurnMetadataHeader`.
- Produces:

```go
type RawResponsesCompactionProtocol string

const (
    RawResponsesCompactionNone RawResponsesCompactionProtocol = ""
    RawResponsesCompactionV1 RawResponsesCompactionProtocol = "responses"
    RawResponsesCompactionV2 RawResponsesCompactionProtocol = "responses_compaction_v2"
)

func DetectRawResponsesCompactionProtocol(header http.Header) RawResponsesCompactionProtocol
```

- [ ] **Step 1: Write failing protocol classification tests**

Add table cases for ordinary turns, exact v1, exact v2, unknown implementations,
missing metadata, and malformed metadata. Use the observed v2 values
`phase: mid_turn` and `strategy: memento`. Reject prefixes and future values.

- [ ] **Step 2: Run tests to verify RED**

Run:

```bash
go test ./internal/adapter/codex -run '^TestDetectRawResponsesCompactionProtocol$' -count=1
```

Expected: build failure because `DetectRawResponsesCompactionProtocol` does not exist.

- [ ] **Step 3: Implement the typed classifier**

Use this shape:

```go
type rawResponsesCompactionMetadata struct {
    RequestKind string `json:"request_kind"`
    Compaction struct {
        Implementation string `json:"implementation"`
        Phase string `json:"phase"`
        Strategy string `json:"strategy"`
    } `json:"compaction"`
}
```

Return a protocol only when `RequestKind == "compaction"` and the implementation
exactly matches a declared constant. Replace the v1 matcher with:

```go
if DetectRawResponsesCompactionProtocol(raw.Header) != RawResponsesCompactionV1 {
    return raw, nil
}
```

- [ ] **Step 4: Run focused and existing compaction tests**

Run:

```bash
go test ./internal/adapter/codex -run 'TestDetectRawResponsesCompactionProtocol|TestRawResponsesCompaction|TestPlanRawResponsesCompaction' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit Task 1**

```bash
git add internal/adapter/codex/raw_compaction_protocol.go internal/adapter/codex/raw_compaction_protocol_test.go internal/adapter/codex/raw_compaction.go
git commit -S -m "Classify native Responses compaction protocols" -m "Co-authored-by: Codex <noreply@openai.com>"
```

---

### Task 2: Route v2 through native pass-through

**Files:**

- Modify: `internal/adapter/codex/raw_compaction.go`
- Modify: `internal/adapter/server_responses_native_test.go`

**Interfaces:**

Consumes exact v2 metadata and produces unchanged request and response bytes.

- [ ] **Step 1: Prove byte-preserving endpoint behavior**

Post the minimal v2 request through `/v1/responses`. Return encrypted compaction
events with one `compaction` item and an empty terminal output array. Assert:

```go
if !bytes.Equal(upstreamRequest, originalRequest) {
    t.Fatal("v2 request changed before persistence proof")
}
if !bytes.Equal(clientResponse, upstreamResponse) {
    t.Fatal("v2 response changed before persistence proof")
}
```

- [ ] **Step 2: Prove malformed v2 layouts use native forwarding**

Use exact valid turn metadata and an invalid layout. Require the upstream to
receive the original request and the client to receive the original response.
The layout parser belongs to the later recovery slice, where it gates mutation.

- [ ] **Step 3: Run Task 2 tests**

Run:

```bash
go test ./internal/adapter/codex ./internal/adapter -run 'TestRawResponsesCompactionV2PassesThrough|TestNativeCodexResponsesCompactionV2' -count=1
```

Expected: PASS.

- [ ] **Step 4: Commit Task 2**

```bash
git add internal/adapter/codex/raw_compaction.go internal/adapter/server_responses_native_test.go
git commit -S -m "Model Responses compaction v2 pass-through" -m "Co-authored-by: Codex <noreply@openai.com>"
```

---

### Task 3: Keep v2 byte-preserving

The foundation does not add synthetic history items or rewrite response bodies.
It preserves request and response bytes for exact v2 metadata, including
malformed v2 layouts. A recovery design must establish a supported persistence
mechanism before it changes this behavior.

---

### Task 4: Verify the v2 foundation

**Files:**

- Modify: `docs/adapter/compatibility.md`
- Test: `internal/adapter/codex/raw_compaction_protocol_test.go`
- Test: `internal/adapter/server_responses_native_test.go`

**Interfaces:**

- Consumes: Tasks 1 through 3.
- Produces: a reviewed v2 foundation.

- [ ] **Step 1: Document protocol behavior**

State these behaviors once:

```text
Native compaction implementations use separate protocol handlers.
Unknown implementations pass through unchanged.
responses_compaction_v2 remains pass-through until plaintext persistence is proven.
```

- [ ] **Step 2: Run final repository gates**

Run:

```bash
make check
go test ./... -count=1
go test -race ./... -count=1
go test -shuffle=on ./... -count=1
```

Expected: every command exits zero.

- [ ] **Step 3: Run strongest-model adversarial review**

Review protocol drift, setup and transcript boundaries, encrypted-item byte
preservation, unknown protocol pass-through, credentials, and capture behavior.
Return blockers to their producing task.

- [ ] **Step 4: Commit documentation changes**

```bash
git add docs/adapter/compatibility.md
git commit -S -m "Document native compaction protocol handling" -m "Co-authored-by: Codex <noreply@openai.com>"
```

- [ ] **Step 5: Preserve the activation boundary**

Keep v2 pass-through until a reviewed recovery design and persistence evidence
authorize a production handler.
