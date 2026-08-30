# Native Responses compaction v2 foundation implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `responses_compaction_v2` a separate, typed, byte-preserving protocol path and prove whether Codex persists a separate plaintext assistant item before production trimming exists.

**Architecture:** A protocol classifier routes `responses` to the existing text-summary handler and `responses_compaction_v2` to a dedicated v2 handler. The first production increment validates captured v2 structure and passes it through unchanged. A throwaway isolated probe then tests plaintext persistence; its result gates a second implementation plan for v2 splitting and injection.

**Tech Stack:** Go, OpenAI Responses Server Sent Events, zstd, SQLite capture fixtures, `httptest`, and real Codex CLI sandbox validation.

**Spec:** [Native Responses compaction v2](../specs/2026-08-30-responses-compaction-v2-design.md)

## Global Constraints

- Keep `responses` text-summary compaction behavior unchanged.
- Keep encrypted v2 compaction items byte-identical.
- Keep v2 request and response bytes unchanged until plaintext persistence passes live validation.
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

### Task 2: Model the captured v2 layout and preserve pass-through

**Files:**

- Create: `internal/adapter/codex/raw_compaction_v2.go`
- Create: `internal/adapter/codex/raw_compaction_v2_test.go`
- Modify: `internal/adapter/codex/raw_compaction.go`
- Modify: `internal/adapter/server_responses_native_test.go`

**Interfaces:**

- Consumes: `RawResponsesRequest`, `NormalizeResponseInputItems`, and `RawResponsesCompactionV2`.
- Produces:

```go
type RawResponsesCompactionV2Layout struct {
    SetupEnd int
    TranscriptStart int
    TriggerIndex int
}

func ParseRawResponsesCompactionV2(
    request RawResponsesRequest,
) (RawResponsesCompactionV2Layout, bool)
```

- [ ] **Step 1: Write failing captured-shape tests**

Use a minimal input fixture with this order:

```json
[
  {"type":"additional_tools","role":"developer"},
  {"type":"message","role":"developer","content":[{"type":"input_text","text":"setup"}]},
  {"type":"message","role":"user","content":[{"type":"input_text","text":"older"}]},
  {"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},
  {"type":"reasoning","summary":[],"encrypted_content":"cipher"},
  {"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"patch"},
  {"type":"custom_tool_call_output","call_id":"call-1","output":[{"type":"input_text","text":"result"}]},
  {"type":"compaction_trigger"}
]
```

Require `SetupEnd == 2`, `TranscriptStart == 2`, and `TriggerIndex == 7`.
Reject missing, nonterminal, or duplicate triggers; missing user transcript;
unpaired tools; wrong phase or strategy; and unknown transcript items.

- [ ] **Step 2: Run tests to verify RED**

Run:

```bash
go test ./internal/adapter/codex -run '^TestParseRawResponsesCompactionV2' -count=1
```

Expected: build failure because `ParseRawResponsesCompactionV2` does not exist.

- [ ] **Step 3: Implement typed layout validation**

Decode `input` as `[]json.RawMessage` and normalize items through the durable
Codex representation. Everything before the first user message is setup.
Require exactly one terminal `compaction_trigger`.

Require this metadata contract:

```text
request_kind = compaction
compaction.implementation = responses_compaction_v2
compaction.phase = mid_turn
compaction.strategy = memento
```

Validate unique call and output identifiers over the complete transcript.
Allow unknown setup items because they remain byte-preserved. Reject unknown
transcript items because later splitting cannot account for them.

- [ ] **Step 4: Add explicit v2 pass-through dispatch**

Use an exact switch:

```go
switch DetectRawResponsesCompactionProtocol(raw.Header) {
case RawResponsesCompactionV1:
    return prepareRawResponsesCompactionV1(raw, settings)
case RawResponsesCompactionV2:
    if _, ok := ParseRawResponsesCompactionV2(raw); !ok {
        return raw, nil
    }
    return raw, nil
case RawResponsesCompactionNone:
    return raw, nil
default:
    return raw, nil
}
```

The v2 branch must not trim or create a response transformer.

- [ ] **Step 5: Prove byte-preserving endpoint behavior**

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

- [ ] **Step 6: Run Task 2 tests**

Run:

```bash
go test ./internal/adapter/codex ./internal/adapter -run 'TestParseRawResponsesCompactionV2|TestNativeCodexResponsesCompactionV2PassesThrough' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit Task 2**

```bash
git add internal/adapter/codex/raw_compaction.go internal/adapter/codex/raw_compaction_v2.go internal/adapter/codex/raw_compaction_v2_test.go internal/adapter/server_responses_native_test.go
git commit -S -m "Model Responses compaction v2 pass-through" -m "Co-authored-by: Codex <noreply@openai.com>"
```

---

### Task 3: Run the isolated plaintext persistence probe

**Files:**

- Create temporarily: `.superpowers/sdd/responses-compaction-v2/probe.md`
- Create temporarily: `.superpowers/sdd/responses-compaction-v2/probe_proxy.go`
- No tracked source changes.

**Interfaces:**

- Consumes: the Task 11 sandbox pattern, existing credentials, and captured v2 events.
- Produces: a verified `PASS` or `FAIL` record with next-request and local-rollout persistence evidence.

- [ ] **Step 1: Record immutable controls**

Record HEAD, credential mode, size, modification time, digest, production PIDs,
fingerprint, and health. Never record credential values.

- [ ] **Step 2: Create the throwaway response-rewriting proxy**

Use this boundary:

```go
type probe struct {
    upstream *url.URL
    client *http.Client
    marker string
}

func (p *probe) ServeHTTP(writer http.ResponseWriter, request *http.Request)
func (p *probe) forward(writer http.ResponseWriter, request *http.Request)
func (p *probe) rewriteCompactionV2(writer http.ResponseWriter, response *http.Response)
```

Forward headers and bodies without logging. Disable redirects. Match only exact
v2 metadata. Forward the encrypted item byte-identically. After its
`response.output_item.done`, emit one synthetic assistant message containing:

```text
<pre-compaction-transcript>
V2-PERSISTENCE-PROBE-<random-id>
</pre-compaction-transcript>
```

Use the next output index and sequence numbers. Emit
`response.output_item.added`, `response.content_part.added`,
`response.output_text.delta`, `response.output_text.done`,
`response.content_part.done`, and `response.output_item.done` before forwarding
`response.completed`. Do not alter the encrypted item or terminal response.

- [ ] **Step 3: Run the isolated daemon and real Codex client**

Use a fresh sandbox root, capture database, binary path, proxy port, and adapter
port. Configure only the sandbox provider to call the proxy. Use existing
credentials unchanged. Force compaction with
`model_auto_compact_token_limit = 1000` and require a shell tool turn after
compaction.

- [ ] **Step 4: Verify persistence**

Require:

```text
client_exit = 0
tool_turn_completed = true
encrypted_item_changed = false
marker_count_in_next_request = 1
marker_count_in_rollout = 1
capture_secret_matches = 0
diagnostic_secret_matches = 0
```

If the marker is missing, duplicated, reordered before the encrypted item, or
the client stops, record `FAIL`. Do not implement v2 trimming.

- [ ] **Step 5: Verify teardown controls**

Stop the sandbox and proxy. Confirm ports close. Confirm credential metadata and
production process evidence match Step 1.

- [ ] **Step 6: Record the outcome**

Append exactly one ledger line:

```text
Task 3: v2 plaintext persistence probe PASS
```

or:

```text
Task 3: v2 plaintext persistence probe FAIL - v2 remains pass-through
```

Do not commit the throwaway proxy or probe report.

---

### Task 4: Verify the v2 foundation

**Files:**

- Modify: `docs/adapter/compatibility.md`
- Test: `internal/adapter/codex/raw_compaction_protocol_test.go`
- Test: `internal/adapter/codex/raw_compaction_v2_test.go`
- Test: `internal/adapter/server_responses_native_test.go`

**Interfaces:**

- Consumes: Tasks 1 through 3.
- Produces: a reviewed v2 foundation and a gate for its follow-up plan.

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
preservation, unknown protocol pass-through, credentials, capture behavior,
and live probe evidence. Return blockers to their producing task.

- [ ] **Step 4: Commit documentation changes**

```bash
git add docs/adapter/compatibility.md
git commit -S -m "Document native compaction protocol handling" -m "Co-authored-by: Codex <noreply@openai.com>"
```

- [ ] **Step 5: Route the follow-up**

If Task 3 passed, write a second implementation plan for production v2 request
splitting and synthetic assistant output. If Task 3 failed, keep v2 pass-through
and require new evidence before selecting another persistence approach.
