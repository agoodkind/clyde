# OpenAI API compatibility

Clyde exposes OpenAI-compatible Chat Completions, Responses, Completions, and Models routes. The OpenAI listener must implement the documented wire contract for every method it advertises. The separate Cursor listener must retain its Cursor-specific behavior.

## Scope and evidence

This design covers the OpenAI resources Clyde advertises. It does not claim support for unrelated OpenAI products such as Files, Audio, or Assistants. Before implementation, record every advertised route and method in a conformance matrix. For each request field, response field, status, and streaming event, record the applicable OpenAI reference, Clyde behavior, provider capability, and acceptance test. Date the reference snapshot so future API changes can be assessed separately.

The initial audit has already found concrete mismatches:

| Surface | Current behavior | Required OpenAI behavior |
| --- | --- | --- |
| Chat usage | The shared usage type omits `completion_tokens_details`. Codex parses upstream reasoning tokens but drops them during conversion. | Report `usage.completion_tokens_details.reasoning_tokens` when the provider reports it. Preserve the distinction between zero and absent data. |
| Responses usage | The Chat-to-Responses conversion writes `reasoning_tokens: 0`. | Report the provider's `output_tokens_details.reasoning_tokens`. Do not invent a zero. |
| Chat streaming | The writer includes usage in a finish chunk and sends a separate usage chunk. The usage field is absent on other chunks. | When `stream_options.include_usage` is true, send one final chunk with empty `choices` and aggregate usage. Set `usage` to null on other chunks. Without that option, omit the aggregate usage chunk. |
| Errors | Shared upstream error mapping converts failures to HTTP 400. | Preserve the documented status and error category on the OpenAI listener. |
| Models | Clyde lists models but does not register model retrieval. | Implement the documented list and retrieval methods, including required model fields. |
| Legacy Completions | The handler converts a prompt to Chat Completions and returns a Chat response. | Return a `text_completion` response and implement the documented request and stream contract for this advertised route. |

The [Chat Completions reference](https://developers.openai.com/api/reference/resources/chat), [Responses reference](https://developers.openai.com/api/reference/resources/responses/methods/create), [Models reference](https://developers.openai.com/api/reference/resources/models), [Completions reference](https://developers.openai.com/api/reference/resources/completions), and [error code guide](https://developers.openai.com/api/docs/guides/error-codes) define the external contract. The matrix must cite the specific reference section used for each assertion.

## Listener contracts

The listener port already identifies OpenAI or Cursor ingress. Select compatibility policy from that established identity. Do not infer the client from a user-agent header, model name, or request body.

The OpenAI listener uses documented OpenAI request validation, response objects, streaming events, and error statuses. An unsupported request feature must either work as documented or return an explicit OpenAI-shaped error. It must not disappear during translation, produce fabricated output, or succeed with only a warning. A provider capability limit is a valid error; a successful response with altered semantics is not.

The Cursor listener retains its current BYOK behavior. Its upstream error mapper converts failures to HTTP 400 with `invalid_request_error` so Cursor displays the provider's diagnostic. Preserve the existing capacity-shed message, compatibility warnings, tool representation, and ingress-specific reasoning rendering. Changes to shared data types may improve truthful usage reporting on both listeners, but OpenAI-only validation or status changes must not alter Cursor requests.

Native Codex Responses requests remain on their existing raw-forwarding path. Preserve unknown fields, upstream status, event order, and native compaction handling on that path. Generic OpenAI projection must not intercept a request already classified as native Codex traffic.

## Usage and streaming

The internal usage value must represent optional provider details without replacing missing values with zero. Codex `output_tokens_details.reasoning_tokens` maps to Chat `completion_tokens_details.reasoning_tokens` and Responses `output_tokens_details.reasoning_tokens`. Other provider adapters map a detail only when the upstream provides it. Aggregate totals retain their existing source and must not be recomputed from an incomplete breakdown.

Chat streaming must obey `stream_options.include_usage` exactly. With the option enabled, every ordinary chunk has `usage: null`, one final chunk has empty `choices` and aggregate usage, and `[DONE]` terminates the stream once. Without the option, no aggregate usage chunk is sent. A stream interrupted before completion may lack the final usage chunk. Responses streaming must emit the documented event types, ordering, and terminal usage for its selected operation; it must not reuse Chat chunk conventions.

## Request and response fidelity

The conformance matrix determines which fields can be implemented locally and which require compatible upstream forwarding. This includes tools, tool choice, structured output, multimodal content, reasoning controls, service tier, storage, truncation, and response lifecycle fields where the selected endpoint documents them. Unsupported combinations receive a field-specific error before a provider request starts. Unknown fields must not be silently discarded when they affect semantics.

The Models resource must return documented list and retrieval shapes. The legacy Completions resource must return its own object and streaming shapes. All currently advertised OpenAI routes remain in scope for repair. Keep Cursor-required behavior on the Cursor listener under its existing compatibility policy.

## Acceptance

Public HTTP tests must exercise each advertised method on both listeners with a local provider server. Assert raw status, headers, JSON presence and absence, SSE event sequence, and terminal behavior. Include an upstream reasoning count, a missing reasoning detail, a nonzero upstream failure, an unsupported request field, and a Cursor BYOK error. Use an official OpenAI SDK against the OpenAI listener for one nonstreaming and one streaming request per applicable resource. The conformance matrix is complete only when every documented behavior in scope is implemented, forwarded without alteration, or rejected with the correct documented error.

Do not describe Clyde as fully OpenAI-compatible until the matrix and public-boundary tests pass. Record unrelated OpenAI products separately from failures within the advertised resources.
