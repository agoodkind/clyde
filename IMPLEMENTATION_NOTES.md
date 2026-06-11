# Implementation Notes

## Summary

The feature-aware wire flavor refactor now records the request feature vector that
Claude CLI used when Clyde captured a wire flavor, persists that vector in the
MITM v2 baseline, loads it into the Anthropic adapter, and selects the learned
interactive flavor by exact request features at egress time.

The selected features are model id, 1M context, thinking mode, structured output
presence, and tool presence. A request with no learned matching flavor fails with
the typed unseeded-flavor path so the adapter can return the operator-actionable
HTTP 503 instead of replaying the wrong beta set.

The old manual beta knobs were removed. Outbound beta assembly now mirrors the
learned Claude CLI flavor, except for the deliberate adapter-wide removal of
`thinking-token-count-2026-05-13` and `redact-thinking-2026-02-12`.

## Files Changed By Area

### MITM capture and schema

- `internal/mitm/request_features.go`
- `internal/mitm/request_features_test.go`
- `internal/mitm/drift_capture.go`
- `internal/mitm/drift_capture_test.go`
- `internal/mitm/schema.go`
- `internal/mitm/schema_v2.go`
- `internal/mitm/snapshot_v2.go`
- `internal/mitm/proxy.go`

### Adapter loader

- `internal/adapter/anthropic/wire_flavors.go`
- `internal/adapter/anthropic/wire_flavors_loader.go`
- `internal/adapter/anthropic/wire_flavors_loader_test.go`
- `internal/adapter/anthropic/wire_baseline_testhelper_test.go`

### Adapter selection

- `internal/adapter/anthropic/wire_flavor_selection.go`
- `internal/adapter/anthropic/client.go`
- `internal/adapter/anthropic/types.go`
- `internal/adapter/anthropic/wire_parity_test.go`
- `internal/adapter/anthropic/wire_shape_test.go`
- `internal/adapter/anthropic/backend/request_builder.go`
- `internal/adapter/anthropic/backend/request_builder_test.go`
- `internal/adapter/anthropic_provider_dispatch.go`
- `internal/adapter/anthropic_provider_dispatch_test.go`

### Adapter beta assembly

- `internal/adapter/anthropic/client.go`
- `internal/adapter/anthropic/beta_suppress_test.go`
- `internal/adapter/anthropic/backend/wire_helpers.go`

### Config

- `internal/config/config.go`
- `internal/config/load_test.go`
- `clyde.example.toml`

### Verification notes

- `IMPLEMENTATION_NOTES.md`

## Checks

- `make fmt`: passed, and `git diff --exit-code` was a no-op afterward.
- `make test`: passed.
- `make lint`: passed.
- `make staticcheck`: not run, and not defined. The Makefile header explicitly
  forbids a project-local `staticcheck` target. Staticcheck runs inside the
  central `lint-golangci` gate that `make build` invokes, so coverage is
  confirmed there. Note: the repository `AGENTS.md` "Common checks" list names
  `make staticcheck`, which contradicts the Makefile; that inconsistency is a
  repo-level follow-up, not part of this refactor.
- `make staticcheck-extra`: passed with `New findings: 0`.
- `make deadcode`: passed with `New findings: 0`.
- `make build`: passed. The build-check substeps reported `vet`,
  `lint-golangci`, `lint-format`, `lint-gocyclo`, `lint-deadcode`,
  `staticcheck-extra`, and `govulncheck` as passing, then built and signed
  `dist/clyde`.

Network was available for the targets that needed it. No target failed due to a
network outage.

## Open Questions And Deviations

No code deviation from `docs/wire-baseline-feature-selection.md` was found during
this verification slice.

No live Cursor, daemon reload, or MITM seeding probe was run. The slice requested
the repository check suite and explicitly prohibited `make deploy`, live daemon
changes, and edits outside this worktree.
