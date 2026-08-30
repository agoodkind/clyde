# Native Responses compaction v2

Date: 2026-08-30
Status: Draft for review

## Result

Clyde treats `responses` and `responses_compaction_v2` as separate native
compaction protocols. The existing `responses` behavior remains unchanged.
The v2 path stays byte-preserving until a real Codex acceptance probe proves
that the client persists an added plaintext assistant item beside the encrypted
compaction item.

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

## Plaintext persistence gate

The v2 response contains encrypted compaction state. Clyde must not alter that
item or assume it can encode compatible encrypted content.

Before production v2 trimming exists, an isolated acceptance probe must answer
one question: does Codex persist a separate plaintext assistant message emitted
after the encrypted compaction item?

The probe uses a throwaway daemon, capture database, and replayable upstream.
It keeps the encrypted item unchanged and adds one assistant message containing
a unique transcript marker. The next Codex request and local rollout artifact
must both retain that marker.

The probe passes only when all conditions hold:

- Codex completes compaction and continues the turn.
- The encrypted compaction item remains byte-identical.
- The added message persists exactly once.
- The next request retains the marker after the compaction item.
- Event identifiers, indexes, and ordering remain valid.
- No credential value appears in captures or diagnostics.

Probe code is throwaway. It does not enter the production branch.

## Production behavior after the probe

If the probe passes, the v2 handler:

1. Preserves setup items and the terminal trigger.
2. Selects a bounded lower set of complete transcript turns.
3. Removes only those raw input items from the upstream compaction request.
4. Renders the removed turns through the generic transcript renderer.
5. Leaves every encrypted compaction event and item unchanged.
6. Emits one additional assistant message item after the encrypted item and
   before `response.completed`.
7. Places the tagged lower transcript in that message.
8. Updates event indexes and identifiers without rewriting unknown frames.

If the probe fails, v2 stays explicit byte-preserving pass-through. Clyde does
not trim the request or mutate the response. The protocol remains represented
as a separate handler so later evidence can enable it without changing v1.

## Streaming and error behavior

Clyde prioritizes incremental delivery for valid compressed streams. It probes
the compressed framing before committing decoded response headers.

After decoded bytes reach the client, a later compression failure terminates
that stream. Clyde cannot rewind already delivered plaintext into the original
compressed response. This late-corruption case does not attempt replay.

Before any decoded byte reaches the client, decoding, parsing, pairing,
rendering, or mutation failure returns the original request or response
unchanged. Unknown Server Sent Events frames remain byte-identical and in
order. Post-candidate buffering stays bounded.

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

## Verification

Behavior tests cover:

- exact protocol classification and unknown-protocol pass-through;
- captured v2 setup, transcript, tool pair, and trigger shapes;
- complete-turn and tool-pair split boundaries;
- unknown or unrenderable removed-item fail-open;
- byte-identical encrypted compaction events;
- synthetic message event ordering and single injection;
- incremental compressed streaming and bounded buffering;
- all four redacted capture stages;
- generic requests and v1 compaction remaining unchanged.

Final verification runs `make check`, full tests, race tests, shuffled tests,
and strongest-model adversarial review. Live validation uses the built-in Codex
route in an isolated daemon. It records credential metadata before and after
without printing values and proves the production daemon remains unchanged.

## Out of scope

- Generating or modifying encrypted compaction content.
- Enabling v2 request trimming before plaintext persistence is proven.
- Replaying a compressed response after decoded bytes already reached the
  client.
- Token rotation, token replacement, production deployment, or production
  daemon restart.
