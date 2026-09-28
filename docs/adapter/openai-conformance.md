# OpenAI conformance matrix

This reference records the OpenAI contract for every route the generic OpenAI listener advertises, and the Clyde behavior verified against it. The generic OpenAI listener is the adapter `port`. The Cursor BYOK listener is `cursor_ingress_port`. Clyde selects the contract from the label the accepted connection receives, never from request headers or body.

## Reference snapshot

The contract comes from these sources, read on 2026-09-27:

- [Chat Completions reference](https://developers.openai.com/api/reference/resources/chat)
- [Responses create reference](https://developers.openai.com/api/reference/resources/responses/methods/create)
- [Models reference](https://developers.openai.com/api/reference/resources/models)
- [Completions reference](https://developers.openai.com/api/reference/resources/completions)
- [Error code guide](https://developers.openai.com/api/docs/guides/error-codes)
- The `openai` Python SDK 3.19.2 Pydantic models, which mark each required response field
- The official OpenAI Go SDK `github.com/openai/openai-go/v3` v3.66.0, which marks each required response field with an `api:"required"` tag

`TestOpenAIGoSDKConformanceCodexProvider` and `TestOpenAIGoSDKConformanceAnthropicProvider` run the Go SDK client against the generic OpenAI listener. They check every response object and stream event against the SDK's required tags and presence metadata.

A disposition is one of four values. Implemented means Clyde produces the documented behavior. Forwarded means Clyde passes the value to a provider that honors it. Rejected means Clyde returns HTTP 400 `invalid_request_error` with the field in `param` before any provider request starts. Deviation means Clyde differs from the reference, and the row states the difference.

## Errors

| Behavior | Reference | Disposition | Test |
| --- | --- | --- | --- |
| An upstream 4xx, 500, 502, 503, or 504 returns the upstream status. Another 5xx becomes 500. A failure with no upstream status uses 429 for rate limits, 401 for auth, 502 for network and generic failures, and 503 for an unavailable backend. | Error code guide | Implemented | `TestOpenAIConformanceUpstreamFailureStatusByListener` |
| `error.type` follows the status: 400, 404, 405, 409, and 422 are `invalid_request_error`, 401 is `authentication_error`, 403 is `permission_error`, 429 is `rate_limit_error`, 503 is `service_unavailable_error`, and other 5xx are `server_error`. | Error code guide, SDK status classes | Implemented | `TestOpenAIConformanceUpstreamFailureStatusByListener` |
| An unknown model returns 404 with code `model_not_found` and param `model`. | Models reference, SDK `NotFoundError` | Implemented | `TestOpenAIConformanceModelsListAndRetrieve`, SDK smoke |
| A mid-stream Chat failure writes one `data: {"error": ...}` frame with the documented type, then one `[DONE]` frame. | Chat Completions streaming | Implemented | `TestOpenAIConformanceChatMidStreamFailure` |
| `error.message` repeats the upstream diagnostic. The `error` object also includes the `clyde` diagnostics member. | Error code guide | Implemented | `TestOpenAIConformanceUpstreamFailureStatusByListener` |

## Models

| Behavior | Reference | Disposition | Test |
| --- | --- | --- | --- |
| `GET /v1/models` returns `object: "list"` and model entries with `id`, `object`, `created`, and `owned_by`. `created` is the Unix time when Clyde loaded the catalog. | Models list | Implemented | `TestOpenAIConformanceModelsListAndRetrieve`, SDK smoke |
| `GET /v1/models/{model}` returns an advertised model or a model a route rule resolves for the surface. | Models retrieve | Implemented | `TestOpenAIConformanceModelsListAndRetrieve`, SDK smoke |
| For a fallback-only model, Clyde sends `GET /models/{model}` to the OpenAI-compatible upstream. Clyde returns a 2xx model object unchanged and returns 404 `model_not_found` for an upstream 404. | Models retrieve | Forwarded | `TestOpenAIConformanceModelsListAndRetrieve` |
| A method other than `GET` returns 405. | Models list | Implemented | `TestOpenAIConformanceModelsListAndRetrieve` |
| `DELETE /v1/models/{model}` deletes a fine-tuned model. | Models delete | Not advertised | None |

## Chat Completions

| Behavior | Reference | Disposition | Test |
| --- | --- | --- | --- |
| A nonstreaming response always writes `choices[].logprobs`, `message.content`, and `message.refusal`, with JSON null for an absent value. | Chat completion object | Implemented | `TestOpenAIGoSDKConformanceCodexProvider` |
| `usage.completion_tokens_details.reasoning_tokens` reports the provider count and is absent when the provider reports none. Codex reports `output_tokens_details.reasoning_tokens`. Anthropic reports `output_tokens_details.thinking_tokens`. | Chat usage object | Implemented | `TestOpenAIConformanceReportsUpstreamReasoningTokens`, `TestOpenAIConformanceOmitsAbsentReasoningDetail`, `TestOpenAIGoSDKConformanceAnthropicProvider` |
| `usage.prompt_tokens_details` reports `cached_tokens` and `cache_write_tokens` when the provider reports them. Anthropic reports `cache_read_input_tokens` and `cache_creation_input_tokens`. | Chat usage object | Implemented | `TestOpenAIConformanceReportsUpstreamReasoningTokens`, `TestOpenAIGoSDKConformanceAnthropicProvider` |
| With `stream_options.include_usage`, every ordinary chunk has `usage: null`, one final chunk has empty `choices` and aggregate usage, and one `[DONE]` ends the stream. | `stream_options.include_usage` | Implemented | `TestOpenAIConformanceChatStreamUsageOptIn`, SDK smoke |
| Without the option, no chunk has a `usage` member and no aggregate chunk is sent. | `stream_options.include_usage` | Implemented | `TestOpenAIConformanceChatStreamWithoutUsageOptIn` |
| `stream_options` without `stream: true` returns 400 with code `invalid_parameter`. | `stream_options` | Rejected | `TestOpenAIConformanceChatRejectsFieldsBeforeProviderRequest` |
| A top-level field outside the documented schema and Clyde's extension fields returns 400 with code `unknown_parameter`. | Create request body | Rejected | `TestOpenAIConformanceChatRejectsFieldsBeforeProviderRequest` |
| `prompt_cache_key`, `prompt_cache_options`, `safety_identifier`, `prediction`, `user`, and `metadata` are accepted. They do not change generated output. | Create request body | Implemented | `TestOpenAIConformanceChatAcceptsDocumentedDefaults` |
| `verbosity`, `web_search_options`, and `moderation` return 400. No provider path exists. | Create request body | Rejected | `TestOpenAIConformanceChatRejectsFieldsBeforeProviderRequest` |
| A field set to its documented default is accepted on every provider. | Create request body | Implemented | `TestOpenAIConformanceChatAcceptsDocumentedDefaults` |

The provider columns show the disposition of each request field that one provider cannot honor. The OpenAI-compatible passthrough provider forwards every field unchanged.

| Field | Codex | Anthropic |
| --- | --- | --- |
| `n` greater than 1 | Rejected | Rejected |
| `temperature` | Rejected unless 1 | Forwarded up to 1, rejected above 1 |
| `top_p` | Rejected unless 1 | Forwarded |
| `max_tokens`, `max_completion_tokens`, `max_output_tokens` | Rejected | Forwarded |
| `stop` | Rejected | Forwarded |
| `presence_penalty`, `frequency_penalty` | Rejected unless 0 | Rejected unless 0 |
| `logit_bias`, `seed`, `audio` | Rejected | Rejected |
| `logprobs: true`, `top_logprobs` above 0 | Rejected | Rejected |
| `modalities` other than `["text"]` | Rejected | Rejected |
| `store: true` | Rejected | Rejected |
| `service_tier` | Forwarded | Rejected unless `auto` or `default` |
| `tool_choice` | Rejected unless `auto` | Forwarded |
| `function_call` | Rejected unless `auto` | Rejected unless `auto` |
| `response_format` | Rejected unless `text` | Forwarded |

## Responses

| Behavior | Reference | Disposition | Test |
| --- | --- | --- | --- |
| The Response object repeats `instructions`, `max_output_tokens`, `metadata`, `parallel_tool_calls`, `previous_response_id`, `reasoning`, `store`, `temperature`, `text`, `tool_choice`, `tools`, `top_p`, `truncation`, and `user`, with the documented default for an omitted field. `store` is always false because Clyde stores no responses. | Response object | Implemented | `TestOpenAIConformanceResponsesStreamEventsAndEcho`, SDK smoke |
| `usage.output_tokens_details.reasoning_tokens` and `usage.input_tokens_details` report the Codex and Anthropic counts listed in the Chat rows. | Response usage object | Implemented | `TestOpenAIConformanceReportsUpstreamReasoningTokens`, `TestOpenAIGoSDKConformanceAnthropicProvider`, SDK smoke |
| Clyde omits a detail that the provider does not report instead of writing an invented zero. The SDK marks `output_tokens_details` and `cache_write_tokens` required. Codex and Anthropic reported both counts in every captured response on 2026-09-27. | Response usage object | Deviation only when a provider omits a count | `TestOpenAIConformanceOmitsAbsentReasoningDetail` |
| The stream starts with `response.created` and `response.in_progress`, finishes each output item before the next item starts, and ends with `response.completed`, `response.incomplete`, or `response.failed`. Every event has `sequence_number`. No `[DONE]` frame is sent. | Responses streaming events | Implemented | `TestOpenAIConformanceResponsesStreamEventsAndEcho`, SDK smoke |
| A reasoning item emits `response.reasoning_summary_part.added` before its text deltas and `response.reasoning_summary_part.done` after `response.reasoning_summary_text.done`. | Responses streaming events | Implemented | `TestOpenAIConformanceResponsesStreamEventsAndEcho` |
| `response.output_text.delta` and `response.output_text.done` have an empty `logprobs` array. | Responses streaming events | Implemented | `TestOpenAIConformanceResponsesStreamEventsAndEcho` |
| Each reasoning segment is its own reasoning output item. A reasoning segment after a message or tool item starts a new item with a new id, in the stream and in the nonstreaming object. | Responses streaming events, Response object | Implemented | `TestOpenAIConformanceResponsesSeparatesReasoningItems` |
| A field the provider omits or overrides returns 400 unless the field is a hint or its value is the documented default. Hints are `prompt_cache_key`, `prompt_cache_options`, `prompt_cache_retention`, `user`, `safety_identifier`, `metadata`, and `stream_options`. | Create request body | Rejected | `TestOpenAIConformanceResponsesRejectsFieldsBeforeProviderRequest`, `TestOpenAIConformanceResponsesAcceptsDocumentedDefaults` |
| A built-in or custom tool the provider cannot run returns 400 for `tools`. | Create request body | Rejected | `TestOpenAIConformanceResponsesRejectsFieldsBeforeProviderRequest` |
| `previous_response_id`, `prompt`, and `conversation` return 400. Clyde stores no responses, prompts, or conversations. | Create request body | Rejected | `TestOpenAIConformanceResponsesRejectsFieldsBeforeProviderRequest` |
| A top-level field outside the documented schema returns 400 with code `unknown_parameter`. | Create request body | Rejected | `TestOpenAIConformanceResponsesRejectsFieldsBeforeProviderRequest` |
| The generic OpenAI listener sends no `X-Clyde-Warning` header and no `clyde.warnings` member. | Create request body | Implemented | `TestOpenAIConformanceResponsesAcceptsDocumentedDefaults` |
| Retrieve, delete, cancel, input items, compact, and input token methods under `/v1/responses/{id}`. | Responses methods | Not advertised | None |

The per-provider field dispositions come from the same catalog as the Cursor listener's [compatibility warnings](compatibility.md).

## Legacy Completions

| Behavior | Reference | Disposition | Test |
| --- | --- | --- | --- |
| A nonstreaming request returns `object: "text_completion"`, a `cmpl-` id, choices with `text`, `index`, `logprobs: null`, and `finish_reason`, and usage. | Completion object | Implemented | `TestOpenAIConformanceLegacyCompletions`, SDK smoke |
| A streaming request returns `text_completion` chunks with incremental `text` and ends with one `[DONE]`. With `stream_options.include_usage`, ordinary chunks have `usage: null` and one final chunk has empty `choices` and usage. | Completions streaming | Implemented | `TestOpenAIConformanceLegacyCompletions`, SDK smoke |
| `prompt` accepts a string or an array with one string. Token arrays and several prompts return 400. | Create request body | Rejected | `TestOpenAIConformanceLegacyCompletions` |
| `suffix`, `echo: true`, `logprobs`, `best_of` above 1, and `n` above 1 return 400. | Create request body | Rejected | `TestOpenAIConformanceLegacyCompletions` |
| Forwarded fields follow the Chat Completions provider columns. | Create request body | Implemented | `TestOpenAIConformanceLegacyCompletions` |

## Cursor and native Codex contracts

The Cursor BYOK listener uses the compatibility contract. It returns upstream failures as HTTP 400 `invalid_request_error` with a typed `upstream_*` code. It sends Responses compatibility warnings, the forced Chat usage chunk, the legacy Chat-shaped `/v1/completions` response, and the model list without `created`. Its usage has no `completion_tokens_details` and no `prompt_tokens_details.cache_write_tokens`. Anthropic usage writes `prompt_tokens_details` only for a cache read. Its Responses usage always reports `input_tokens_details` and a zero `reasoning_tokens`. `TestOpenAIConformanceUpstreamFailureStatusByListener`, `TestOpenAIConformanceChatStreamWithoutUsageOptIn`, `TestOpenAIConformanceReportsUpstreamReasoningTokens`, `TestAnthropicReportedUsageKeepsCursorUsageBytes`, `TestOpenAIConformanceModelsListAndRetrieve`, and the Responses warning tests verify these behaviors.

Clyde classifies a Responses request for native Codex forwarding when the request has valid `X-Codex-Turn-Metadata` and its model resolves to Codex. Clyde forwards that request body, its unknown fields, the upstream status, and the response bytes unchanged on every listener. Clyde encodes adapter-side errors for that request with the compatibility contract. `TestOpenAIListenerKeepsNativeCodexForwardingContract` and `TestOpenAIListenerKeepsNativeCodexTransportFailureContract` verify these behaviors.

## Out of scope

Files, Audio, Images, Embeddings, Moderations, Batches, Fine-tuning, Assistants, Threads, Vector stores, Realtime, and stored Chat Completions methods are OpenAI products the adapter does not advertise.

Usage notice text insertion is outside this matrix by the 2026-09-27 scope decision.
