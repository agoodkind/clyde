# Adapter Compatibility Warnings

The generic Responses projection forwards what the resolved provider can honor
and warns for every request field it drops or overrides. A generic
`/v1/responses` request never fails just because it carries a field or tool type
the backend cannot use.

## Native Codex boundary

A request with valid `X-Codex-Turn-Metadata` uses authenticated native raw
forwarding only when its top-level model resolves to Codex. That path does not
project fields or emit compatibility warnings. It preserves unknown fields,
tools, identifiers, and upstream status. It preserves response bytes unless
native `responses` compaction is enabled and the request carries the exact
compaction metadata. That v1 case mutates only the final assistant summary. Clyde
supplies its configured Codex OAuth token and account identity instead of
trusting inbound credentials.

Native compaction implementations use separate protocol handlers, and unknown
implementations pass through unchanged. `responses_compaction_v2` remains
byte-preserving pass-through. A wire-equivalent Codex 0.151.0 probe preserved
the encrypted item but discarded the added plaintext assistant item. Activating
v2 transcript recovery requires a supported persistence mechanism and a
reviewed production handler.

Invalid metadata and non-Codex models stay on the generic projection path. The
field and tool rules below apply to that path.

## Warning contract

A warning is a typed `CompatibilityWarning` with four fields: `code`, `param`,
`disposition`, and `message`. The `code` is `field_omitted`, `field_overridden`,
or `tool_unsupported`. The `param` is the request field name, such as
`temperature` or `tools`. The `disposition` is `omitted` or `overridden`. The
`message` is a generic sentence that never carries a request value, a prompt, or
a credential.

The adapter surfaces the same warnings on three surfaces:

- Repeated `X-Clyde-Warning` response headers, one compact JSON object per
  warning.
- The `clyde.warnings` array on the non-streaming Responses response object,
  which is present only when there is at least one warning.
- The first streaming frame, where the warnings ride on the `clyde.warnings`
  array of the `response.created` and `response.in_progress` snapshots. The
  terminal `response.completed` object stays warning free.

Warnings appear in a fixed request-field order, dedupe by code and param, and
cap at 32 entries and 8 KiB of summed header bytes. The order and caps are
deterministic so the same request always yields the same warning list.

The [`internal/adapter/compat`](../../internal/adapter/compat/compat.go)
package computes the set and owns the ordering, dedupe, and cap rules. The
[compat tests](../../internal/adapter/compat/compat_test.go) enforce the
contract, and
[`server_responses_compat_test.go`](../../internal/adapter/server_responses_compat_test.go)
covers the header, object, and stream surfaces end to end.

## Per-provider field dispositions

Each backend reads its own column of the field catalog. Codex and Anthropic
drop or override different fields, and the Claude backend shares the Anthropic
column. The passthrough backend forwards every field unchanged and warns for
nothing.

| Request field | Codex | Anthropic |
| --- | --- | --- |
| `max_output_tokens` | omit and warn | carry through |
| `temperature` | omit and warn | carry through |
| `top_p` | omit and warn | carry through |
| `stop` | omit and warn | carry through |
| `store` | override to false and warn | omit and warn |
| `include` | carry through | omit and warn |
| `service_tier` | carry through | omit and warn |
| `truncation` | omit and warn | omit and warn |
| `prompt_cache_retention` | omit and warn | omit and warn |

The Anthropic backend carries `temperature`, `top_p`, and `stop` through by
translating them into the Messages request, clamping `temperature` to
Anthropic's `[0,1]` range and mapping `stop` onto `stop_sequences`. A field the
client omits leaves the request unchanged. The
[catalog](../../internal/adapter/compat/catalog.go) is the source of truth for
each field's disposition.

A field warns only when the request actually carries it. An absent field and an
explicit `null` both suppress the warning, while a zero, `false`, or empty value
still warns.

## Unsupported Responses tools

A `/v1/responses` request whose `tools` array carries non-function tools keeps
the function tools, drops the rest, and warns with code `tool_unsupported`
rather than rejecting the whole request. OpenAI built-in tools such as
web_search, file_search, computer_use, and mcp, plus custom tools, are the
dropped types. The
[`responses_tools.go`](../../internal/adapter/openai/responses_tools.go)
splitter classifies the tools, and its
[tests](../../internal/adapter/openai/responses_tools_test.go) pin the
keep-and-warn behavior.
