# Feature-aware wire flavor selection

Northstar for making the Anthropic egress beta header correct per model, with zero config.

## Problem

Egress ignores the request model. `selectInteractiveFlavor` picks the smallest interactive flavor and replays its beta set on every request. A Sonnet request gets a Fable flavor that carries `context-1m-2025-08-07`. The account blocks that beta on Sonnet, so the request fails with `rate_limit_error: "Usage credits are required for long context requests"`.

Verified 2026-06-09 from `mitm/capture.db`: `context-1m` rides 680/682 Fable requests, 65/67 Opus, and 0/48 Haiku. The beta tracks the model, not the request size. claude-cli already gates it per model and per account. Clyde does not.

## Goal

Select the learned flavor that matches the request's model and features. Replay its exact beta set. The header becomes whatever claude-cli sent for that combination on this account. No hand-maintained beta config.

## Changes

1. Flavor identity gains a feature vector: model, context tier, thinking mode, structured-output, tools. Source the values from the captured request body and headers.
2. Each learned flavor records the feature vector it was seen with, next to its beta set and identity headers.
3. Egress builds the resolved request's feature vector and selects the matching flavor. This replaces `selectInteractiveFlavor`.
4. No matching flavor returns HTTP 503 with an actionable message: run claude-cli once with this model through the MITM to seed it. It fails loud instead of guessing.
5. Plaintext thinking moves from config into code. Clyde always strips the two thinking-redaction betas (`thinking-token-count-2026-05-13`, `redact-thinking-2026-02-12`) so Cursor shows reasoning.
6. Remove `per_context_betas` and `beta_suppress`. Nothing in `config.toml` manages betas.

## Files

- `internal/mitm/flavors.go`: feature vector in the flavor signature.
- `internal/mitm/snapshot.go` and the v2 baseline schema: capture and persist the feature vector per flavor.
- `internal/adapter/anthropic/client.go`: feature-aware selection; `activeFlavor` takes the resolved request.
- `internal/adapter/anthropic/wire_flavors_loader.go`: project the new fields; built-in thinking-redaction strip.
- `internal/config/config.go`: drop `PerContextBetas` and `BetaSuppress`.

## Verify

- A Sonnet request selects a Sonnet flavor, never a Fable flavor.
- A 200k Sonnet request carries no `context-1m`. A 1M Fable or Opus request carries it.
- An un-seeded model returns the 503.
- Thinking renders as plaintext in Cursor with no config set.

## Operate

When a new model appears, run one real claude-cli session with it through the MITM. The flavor is learned. Clyde mirrors it.
