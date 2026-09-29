# OpenAI API compatibility implementation plan

## Goal

Every OpenAI resource advertised on Clyde's OpenAI listener follows the applicable OpenAI request, response, error, and streaming contract. The Cursor listener retains its current BYOK behavior, and native Codex Responses forwarding remains unchanged.

## Current behavior

`internal/adapter/server_routes.go` registers Chat Completions, Responses, Completions, and model listing on a shared mux. The configured listener port identifies OpenAI or Cursor ingress. `internal/adapter/openai/error_envelope.go` and `internal/adapter/family_error_mapping.go` map upstream failures to HTTP 400. `internal/adapter/codex/protocol_helpers.go` drops parsed reasoning tokens. `internal/adapter/openai/responses_response.go` writes a zero reasoning count. `internal/adapter/provider_writer.go` sends usage on a finish chunk and a separate usage chunk.

## Constraints

- Preserve the Cursor listener's HTTP 400 `invalid_request_error` mapping and capacity-shed wording. Preserve its warning-based compatibility behavior and ingress-specific rendering.
- Preserve native Codex raw Responses forwarding, including unknown fields, upstream status, event order, and compaction handling.
- Select policy from the existing listener identity. Do not select it from request text or client headers.
- Reject unsupported semantic fields on the OpenAI listener before dispatch. Do not silently discard them or fabricate provider details.
- Add public HTTP regression tests for observable behavior. Use a local provider server and assert raw wire output. Do not use static source assertions.

## Tasks

### 1. Establish the conformance matrix

Files:
- Create: `docs/adapter/openai-conformance.md`
- Inspect: `internal/adapter/server_routes.go`, `internal/adapter/server_dispatch.go`, `internal/adapter/server_responses.go`, `internal/adapter/openai/types.go`, `internal/adapter/openai/responses_request.go`, `internal/adapter/openai/responses_response.go`

Steps:
1. Record every advertised OpenAI route and method. Include required and optional request fields, response fields, statuses, headers, and SSE events.
2. Cite the dated OpenAI reference section for each contract item. Record observed Clyde behavior and the selected provider's capability.
3. Mark each item for local implementation, exact upstream forwarding, or an explicit error for a provider-specific unsupported feature. Do not mark an item complete from code inspection alone.

Verification:
- Inspect the matrix against the registered mux and the official references. Every advertised method and every documented behavior within its scope has one disposition and one public-boundary test target.

### 2. Separate OpenAI and Cursor policy

Files:
- Modify: `internal/adapter/server_routes.go`, `internal/adapter/server_dispatch_ingress.go`, `internal/adapter/family_error_mapping.go`, `internal/adapter/openai/error_envelope.go`
- Modify: `AGENTS.md` if its all-ingress HTTP 400 instruction remains present
- Modify tests: `internal/adapter/server_error_boundary_test.go`, `internal/adapter/ingress_label_test.go`

Steps:
1. Pass the existing listener identity into response and error policy selection. Keep provider and family types in their current packages.
2. Apply documented OpenAI statuses and error objects on the OpenAI listener. Keep the current Cursor error shape and capacity-shed wording on the Cursor listener.
3. Scope repository guidance about HTTP 400 mapping to Cursor ingress. Update the source of any generated guidance before regeneration.

Verification:
- Run: `go test ./internal/adapter/...`
- Expect: The same upstream failure produces its OpenAI status on the OpenAI listener and the existing HTTP 400 diagnostic on the Cursor listener. Native Codex raw forwarding retains the upstream status.

### 3. Preserve usage details and correct Chat streaming

Files:
- Modify: `internal/adapter/openai/types.go`, `internal/adapter/codex/protocol_helpers.go`, `internal/adapter/openai/responses_response.go`, `internal/adapter/provider_writer.go`, `internal/adapter/server_dispatch.go`
- Modify tests: `internal/adapter/provider_writer_test.go`, `internal/adapter/responses_end_to_end_test.go`

Steps:
1. Make reasoning and other provider usage details optional in the shared value. Map present upstream values to the endpoint-specific OpenAI fields.
2. Remove the hardcoded Responses reasoning zero. Preserve absent detail as absent; preserve a reported zero as zero.
3. Respect `stream_options.include_usage`. Send one aggregate usage chunk only when requested. Set `usage: null` on ordinary Chat chunks and terminate with one `[DONE]`.

Verification:
- Run: `go test ./internal/adapter/...`
- Expect: Public HTTP responses report nonzero reasoning usage from a local Codex provider. An absent upstream detail is not reported as zero. Chat streams contain the documented usage chunk sequence with and without opt-in.

### 4. Complete Responses and Chat request fidelity

Files:
- Modify: `internal/adapter/openai/responses_request.go`, `internal/adapter/openai/responses_response.go`, `internal/adapter/openai/responses_events.go`, `internal/adapter/server_responses.go`, `internal/adapter/server_dispatch.go`, `internal/adapter/ingress_registration.go`
- Modify tests: `internal/adapter/responses_end_to_end_test.go`, `internal/adapter/server_responses_compat_test.go`, `internal/adapter/handle_test.go`

Steps:
1. Implement or exactly forward each request and output behavior recorded in the matrix. Keep native Codex classification ahead of generic projection.
2. Reject unsupported semantic fields explicitly on the OpenAI listener before sending a provider request. Retain Cursor compatibility warnings on the Cursor listener.
3. Match Responses event order, terminal status, and terminal usage to the documented operation. Keep Chat and Responses event formats separate.

Verification:
- Run: `go test ./internal/adapter/...`
- Expect: Public HTTP tests pass for supported fields, unsupported-field errors, both listeners, and native Codex passthrough. No OpenAI request succeeds after a semantic field was discarded.

### 5. Complete Models and legacy Completions

Files:
- Modify: `internal/adapter/server_routes.go`, `internal/adapter/server_dispatch.go`, `internal/adapter/openai/types.go`
- Modify tests: `internal/adapter/server_models_test.go`, `internal/adapter/handle_test.go`

Steps:
1. Implement model retrieval and required model fields. Apply the documented missing-model error.
2. Return the legacy `text_completion` object and stream events for `/v1/completions`. Implement documented request fields or reject unsupported combinations explicitly.
3. Complete every currently advertised route. Preserve any Cursor-required behavior on the Cursor listener.

Verification:
- Run: `go test ./internal/adapter/...`
- Expect: Model list and retrieval, missing-model errors, and nonstreaming and streaming legacy Completions match the matrix at the public HTTP boundary.

### 6. Verify the full advertised surface

Files:
- Modify: `docs/adapter/openai-conformance.md`, `docs/adapter/compatibility.md`, `docs/cursor.md` only where current behavior changed
- Add or modify: public HTTP tests under `internal/adapter/`

Steps:
1. Run the complete adapter and repository checks. Exercise one nonstreaming and one streaming request per applicable resource with an official OpenAI SDK configured for Clyde's OpenAI listener.
2. Exercise the existing Cursor BYOK error, warning, tool, and reasoning cases on the Cursor listener. Exercise a native Codex Responses request with unknown fields and an upstream error.
3. Fill every matrix result with a test reference and observed outcome. Document unrelated OpenAI products separately from failures inside advertised resources.

Verification:
- Run: `go test ./internal/adapter/...`
- Run: `make check`
- Expect: Every in-scope matrix item passes its public-boundary assertion. Cursor behavior and native Codex forwarding remain unchanged. No full-compatibility claim is published while an advertised method has an unresolved contract failure.
