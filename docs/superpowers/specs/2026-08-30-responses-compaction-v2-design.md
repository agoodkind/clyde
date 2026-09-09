# Native Responses compaction v2

Date: 2026-08-30
Status: Approved

## Result

Clyde treats `responses` and `responses_compaction_v2` as separate native
compaction protocols. The existing `responses` behavior remains unchanged.
The v2 path keeps the encrypted compaction response byte-identical. Clyde
stores the removed lower transcript until the first later regular assistant
response, forces that transcript into intervening upstream requests, then
appends it once to the regular response so Codex persists it naturally.

No token is rotated, replaced, or written. No production daemon changes during
validation.

## Why v2 needs a separate path

The live Codex 0.150.1 capture establishes a different contract from the text
summary flow:

- Turn metadata reports `request_kind: compaction`,
  `implementation: responses_compaction_v2`, `phase: mid_turn`, and
  `strategy: memento`.
- The request ends with `compaction_trigger`, not a synthesized user prompt.
- Mid-turn reasoning and a custom tool call and output appear before the
  trigger.
- The response emits an encrypted `compaction` item.
- The response has no assistant summary text to modify.
- The next normal request carries the encrypted compaction item forward.

The current text-summary algorithm requires a final user prompt and assistant
text output. Treating v2 as another matcher value would therefore select the
wrong request boundary and find no valid response target.

## Protocol classification

One native compaction dispatcher classifies the turn metadata before any
request mutation:

| Implementation | Handler |
| --- | --- |
| `responses` | Existing text-summary split and final-assistant injection |
| `responses_compaction_v2` | Dedicated encrypted-compaction handler |
| Missing or unknown | Byte-preserving native pass-through |

Each handler owns its request validation, split plan, response mutation, and
fail-open rules. The handlers do not share assumptions about prompts or output
items.

## V2 request model

The v2 handler uses a typed envelope with three regions:

1. The setup region contains developer instructions and tool declarations.
   Clyde always preserves it.
2. The transcript region contains user turns, reasoning, assistant messages,
   and tool exchanges.
3. The terminal region contains exactly one final `compaction_trigger`.
   Clyde preserves its raw bytes and position.

The handler rejects mutation unless every input item maps through the durable
Codex representation and every tool call has one unique matching output.
Unknown setup items remain allowed because Clyde preserves them. Unknown items
inside a candidate removed turn disable trimming.

A removable turn starts at one user message and includes its reasoning, tool
exchanges, and final assistant result. The split uses the existing recent
fraction and token caps. It never cuts through a turn or tool pair.

## Validated Codex behavior

The v2 response contains encrypted compaction state. Clyde does not alter that
item or assume it can encode compatible encrypted content.

A wire-equivalent Codex 0.151.0 probe established this boundary:

- Codex uses v2 only when the built-in `openai` provider points at Clyde through
  `openai_base_url`. A custom `Clyde` model provider reports remote compaction
  as unsupported and uses v1.
- Codex accepts exactly one encrypted compaction output item.
- Codex discards additional plaintext items from the v2 compaction response.
- Codex rebuilds v2 history from its client-local pre-request history, then
  appends the encrypted compaction item.
- A plaintext assistant item added to the v2 response appeared zero times in
  the next request and zero times in the exact rollout.
- The encrypted item remained byte-identical and the client completed the turn.

The limiter is the Codex v2 collector, not the upstream service. Response-time
plaintext injection therefore cannot persist on compaction turn N.

## N to N+1 recovery

The v2 handler reuses the existing normalizer, complete-turn selector,
tool-pair validator, transcript renderer, transcript tags, and regular response
transformer. It does not add another transcript parser or renderer.

On compaction turn N:

1. Validate the captured v2 setup, transcript, and terminal trigger regions.
2. Select the bounded lower transcript on complete turn and tool-pair
   boundaries.
3. Render the lower transcript with the generic transcript renderer.
4. Remove only the selected raw transcript items from the upstream compaction
   request.
5. Preserve setup items, the terminal trigger, tools, headers, and unrelated
   body fields.
6. Forward the encrypted compaction response byte-identically.
7. After Clyde copies the unchanged successful v2 response to the client, arm
   one bounded pending recovery entry.

After N:

1. Match later regular requests by the exact Codex thread and compacted window.
2. Insert one synthetic assistant history item containing the tagged lower
   transcript immediately after the encrypted compaction item in the upstream
   request.
3. For a regular final-answer request, atomically reserve the matching entry
   before request injection. A same-thread overlapping request cannot reserve
   the same entry, so it passes through without a second append.
4. Repeat the request injection while the pending entry remains armed. Codex
   does not persist these request-only copies.
5. Wait through tool-call-only and failed responses.
6. Append the tagged transcript once to the first successful regular final
   assistant response by reusing the existing response transformer.
7. Release the lease when mutation or client delivery fails. Clear the entry
   only after Clyde delivers the mutated response to the client.
8. From N+2 onward, Codex resends the modified regular response naturally.

The pending registry is a process-local, bounded, time-limited staged recovery.
It stores no credentials or encrypted content. It is intentionally not durable
persistence. A daemon restart, expiry, or capacity eviction after trimming
forgoes recovery and fails open to native v2 behavior. This boundary avoids
persisting plaintext recovery state, and it never changes an unmatched request
or response.

Client delivery is the completion boundary. Clyde cannot observe whether Codex
then durably persists its local history, so it does not claim that outcome.
The later natural N+2 resend validates persistence when it occurs.

## Streaming and error behavior

Clyde preserves incremental delivery for valid compressed streams. The v2
compaction response remains unchanged. The later regular response uses the
existing bounded streaming transformer.

After decoded bytes reach the client, a later compression failure terminates
that stream. Clyde cannot rewind already delivered plaintext into the original
compressed response. This late-corruption case does not attempt replay.

Before any decoded byte reaches the client, decoding, parsing, pairing,
rendering, correlation, or mutation failure returns the original request or
response unchanged. Unknown Server Sent Events frames remain byte-identical
and in order. Post-candidate buffering stays bounded.

A pending recovery remains armed when a regular response has no final assistant
text, ends unsuccessfully, cannot be mutated safely, or fails client delivery.
It clears only after one successful appended response reaches the client.

## Capture and security

The four capture stages remain distinct:

| Stage | Stored representation |
| --- | --- |
| Ingress request | Original decoded client request, redacted |
| Upstream request | Final transformed request, decoded and redacted |
| Upstream response | Raw decoded upstream response, redacted |
| Client response | Final decoded client response, redacted |

Compressed capture copies decode before redaction. Decode errors and decoded
bodies above the capture limit store only a redaction marker. Sensitive headers
are removed before decoding begins.

## Configuration

V2 reuses the existing reorient enable flag, recent fraction, token cap,
context fraction, and byte ratio. It adds no configuration keys.

Codex enables v2 through its built-in `openai` provider with
`openai_base_url` pointed at Clyde. The custom provider configuration remains
valid for v1 compatibility.

## Verification

Behavior tests cover:

- exact protocol classification and unknown-protocol pass-through;
- captured v2 setup, transcript, tool pair, and trigger shapes;
- complete-turn and tool-pair split boundaries;
- unknown or unrenderable removed-item fail-open;
- byte-identical encrypted compaction events;
- N+1 request injection after the encrypted compaction item;
- repeated request injection through tool-call-only responses;
- synthetic regular-response event ordering and one transcript append to the
  first successful final assistant response;
- natural N+2 resend with no further forced request injection;
- same-thread lease exclusion, concurrent thread isolation, expiry, response
  failure, client-delivery failure, and restart fail-open;
- incremental compressed streaming and bounded buffering;
- all four redacted capture stages;
- generic requests and v1 compaction remaining unchanged.

Final verification runs `make check`, full tests, race tests, shuffled tests,
and strongest-model adversarial review. Live validation uses the built-in Codex
route in an isolated daemon. It records credential metadata before and after
without printing values and proves the production daemon remains unchanged.

## Out of scope

- Generating or modifying encrypted compaction content.
- Adding another Codex transcript parser or transcript renderer.
- Persisting pending plaintext recovery across daemon restarts.
- Replaying a compressed response after decoded bytes already reached the
  client.
- Token rotation, token replacement, production deployment, or production
  daemon restart.
