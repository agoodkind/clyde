# Adapter Model Routing

Clyde routes adapter requests through one declarative model catalog. Exact
entries describe known capabilities, while ordered routes claim future model
IDs without advertising speculative details.

## Resolve a model

Clyde resolves one model request across the Cursor, generic OpenAI-compatible,
and native Anthropic ingress surfaces. Each route rule names the surfaces where
its claim applies.

For a nonempty model ID, resolution follows this order:

1. Resolve an exact model declaration or alias.
2. Select the first matching route rule for the ingress surface.
3. Use the configured OpenAI-compatible fallback.
4. Return a model error.

The `default_model` applies only when the caller omits the model. It never
replaces an unknown nonempty model ID.

A published route match is final. Clyde does not continue to a later route or
the fallback. Semantic validation rejects route configuration for a disabled
provider before publishing the catalog. Native Anthropic requests must also
resolve to the Anthropic provider.

## Advertise known models

The `/v1/models` response contains only advertised exact catalog IDs and
aliases for enabled providers. Wildcard matches route immediately but never
appear in that response.

An exact model uses its profile for context, output, reasoning, tool, vision,
and thinking capabilities. Clyde validates caller values against that profile.
The profile may map a public reasoning effort to a provider wire value without
changing the caller-facing capability name.
Transport-specific context limits report the same transport Clyde uses for the
request.

A wildcard match preserves the requested wire model and caller-provided
controls. Its tools, vision, context, and output capabilities remain unknown,
so Clyde does not reject the request using invented defaults.

## Configure the catalog

TOML loading is permissive. Clyde ignores keys it does not recognize, including
misspelled, retired, unsupported, and legacy model-routing keys. A misspelled
required field still leaves its recognized catalog entry incomplete, so
semantic validation rejects that entry.

Malformed TOML still prevents loading. Permissive decoding applies to keys, not
to invalid TOML syntax.

Recognized catalog entries still receive semantic validation. Clyde rejects
invalid references, limits, aliases, defaults, provider claims, and route
patterns before publishing a registry. Ignored legacy keys cannot restore
legacy routing because only the declarative catalog feeds the registry.

Model declarations own their pricing. Changes to models, profiles, routes,
aliases, and model-local pricing hot-apply as one catalog. Clyde keeps the
current registry when a replacement catalog fails validation.

Use [the example configuration](../../clyde.example.toml) for the current TOML
shape. The following tests enforce the catalog contract without duplicating it
here:

- [Configuration loading and validation](../../internal/config/model_catalog_test.go)
- [Registry resolution and advertisement](../../internal/adapter/model/catalog_registry_test.go)
- [Codex provider request shaping](../../internal/adapter/codex/request_builder_test.go)
- [Anthropic provider request shaping](../../internal/adapter/anthropic/backend/request_builder_test.go)
- [End-to-end routing through fake providers](../../internal/adapter/model_routing_end_to_end_test.go)
- [Transport-specific capability reporting](../../internal/adapter/codex/capabilities_test.go)
- [Transport-aware model listing](../../internal/adapter/server_models_test.go)
- [Hot-apply change classification](../../internal/config/change_class_test.go)
- [Atomic catalog hot apply](../../internal/adapter/apply_config_test.go)
- [Model-local pricing hot apply](../../internal/daemon/config_apply_pricing_test.go)

## Serve the Responses API

The adapter serves the OpenAI Responses API at `POST /v1/responses` on the same
OpenAI-compatible route family and bearer auth as `/v1/chat/completions`.
Generic requests use a typed projection into the shared chat pipeline. They run
the same resolver, preflight, provider dispatch, and compatibility warnings as
chat requests.

An authenticated native Codex request takes a narrower path. It must carry
valid `X-Codex-Turn-Metadata`, and its top-level model must resolve to Codex.
Clyde then forwards the native Responses body and unknown fields without typed
projection. It replaces inbound credentials with the configured Codex OAuth
token and account identity, strips hop-by-hop headers, and refreshes OAuth once
after an upstream 401 or 403. Requests outside that exact match keep using the
generic projection path.

A non-streaming request returns a Responses response object. A streaming request
returns the Responses SSE event sequence with named lifecycle events, from
`response.created` and `response.in_progress` through `response.completed` or
`response.failed`, and it never emits `data: [DONE]`. One `resp_` id stays stable
across the stream and the terminal object. The
[`handleResponses`](../../internal/adapter/server_responses.go) handler and the
[streaming writer tests](../../internal/adapter/responses_writer_test.go) hold
the behavior, and the [event names](../../internal/adapter/openai/responses_events.go)
list the full lifecycle set.

The adapter also reports which request fields and tool types the resolved
provider cannot honor. See [adapter compatibility warnings](compatibility.md)
for the warning contract, the per-provider field dispositions, and the
unsupported-tool behavior.

When `capture_ingress` is enabled, the daemon opens the shared capture database
even if MITM listeners are disabled. A native request records the ingress
request, transformed upstream request, raw upstream response, and client
response. Capture copies remove adapter and OAuth tokens, account identifiers,
and cookies. Forwarded bytes remain unchanged.
