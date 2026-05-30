# Wire baseline: learn the native client shape, match it on egress

## Purpose

Clyde fronts the native providers as an OpenAI-compatible endpoint for Cursor (BYOK).

To the provider backend, clyde's outbound request must look like the real native client (claude-code to Anthropic, codex-cli to the codex backend). If it does not, the backend can reject it, bill it on the wrong plan, or miss the prompt cache.

Clyde learns each native client's exact wire shape from real traffic captured by the MITM proxy, stores it as a per-upstream baseline, and projects that baseline onto the egress request when a Cursor request comes in.

## Capture and learn

The MITM proxy captures real claude-code and codex-cli requests.

A drift-scoped writer records each native request as a redacted `http_request` record under `<captureRoot>/drift/<upstream>.jsonl`: header names with secrets masked, body reduced to a `{keys, body_type}` summary, attestation captured verbatim. No raw prompt text is stored.

The baseline refresher (`internal/mitm/baseline_refresher.go` schedules and debounces, `internal/mitm/baseline_refresh.go` extracts and writes) turns those records into a per-upstream baseline `reference-v2.toml` under `<state>/clyde/mitm-baselines/<upstream>/`. Upstreams and their `include_ua` filters are set under `[mitm.drift.upstreams.<name>]`. Learning happens automatically from live traffic; no manual step is required.

## Project onto egress

At request time the adapter loads the baseline and shapes the outbound request to match.

- Anthropic: `internal/adapter/anthropic/client.go` loads the claude-code baseline (`WireFlavorsLoader`) and sets User-Agent, `anthropic-beta`, `anthropic-version`, the static identity headers, the body field shape, and the caching markers from it. A missing or invalid baseline returns HTTP 503 (`wire_baseline_unavailable`), so no request goes out without learned identity.
- Codex: the codex transport projects the codex-cli baseline onto the outbound originator, `openai-beta`, User-Agent, and body field order, falling back to compiled-in constants until a codex baseline has been learned.

## Drift sweep

The daemon runs a periodic compare-only check (`internal/mitm/drift_periodic.go`, spawned from `internal/daemon/run.go`). Each tick diffs fresh capture against the saved baseline and appends the structured outcome to `<drift_log_dir>/<upstream>.jsonl`. Divergence is logged at warn as `mitm.drift.periodic_diverged` under the `providers.mitm.wire` concern. A single upstream's failure (missing reference, empty transcript) is logged and does not stop the loop.

The sweep is observability only. The refresher re-learns the baseline automatically, so the live shape is tracked without manual action; the sweep just reports when capture and baseline disagree.

## What clyde cannot reproduce

Some fields are computed by the native client and cannot be validly forged:

- claude-code: the billing attestation hash in the first system block (`cch=...`).
- codex: the `x-oai-attestation` header.

Clyde captures and replays the last-seen value best-effort. A replayed attestation can be stale and rejected by the backend. Clyde never fabricates a fake-valid one.

## Config

- `[mitm.drift].enabled = true` turns on both the learn loop and the drift sweep. With it false or absent, both return immediately.
- `[mitm.drift.upstreams.claude-code]` and `[mitm.drift.upstreams.codex-cli]` set each upstream's `reference` path and `include_ua` filter.
- The anthropic adapter reads the `claude-code` reference as its egress baseline (`internal/daemon/run.go` resolves it into the adapter config).
- Config shape lives in `internal/config/mitm_config.go` (`MITMDriftConfig`). See `clyde.example.toml` for a populated block.

## Coverage

- Anthropic headers: baseline-driven and enforced (503 on missing).
- Anthropic body and caching: see the request builder under `internal/adapter/anthropic/backend/`.
- Codex headers and body: baseline-driven with constant fallback.
- Attestation: best-effort replay for both providers, per the limit above.

## Seed a baseline manually

The baseline is normally learned automatically. This escape hatch seeds it from a JSONL file of captured wire records you already have.

```
clyde mitm seed-baseline --upstream claude-code --from <captured-wire.jsonl>
```

`--from` is a JSONL file of captured MITM wire records, the same `http_request` / `ws_*` records the auto-learn loop reads (for example the drift writer's `<captureRoot>/drift/<upstream>.jsonl`). The command reads that file, runs the extractor over it, and writes `reference-v2.toml` under the default baseline root for the upstream (`<XDG_STATE_HOME>/clyde/mitm-baselines/<upstream>/reference-v2.toml`). The provider filter is derived from the upstream name. Pass `--output` to write elsewhere (the file must be named `reference-v2.toml`), and `--include-ua` / `--exclude-ua` to scope which captured caller flavor seeds the baseline (for example `--include-ua claude-cli` to keep only the upstream CLI and drop other clients sharing the proxy).
