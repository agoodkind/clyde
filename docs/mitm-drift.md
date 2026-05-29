# MITM baseline / drift loop

The drift loop closes the cycle `capture -> baseline refresh -> drift log`.

- Every captured request to a known upstream (for example `claude-code`,
  `codex-cli`) debounces a baseline refresh against the upstream's pinned
  reference snapshot (`internal/mitm/baseline_refresher.go`).
- The daemon also runs a periodic compare-only drift check
  (`internal/mitm/drift_periodic.go`, spawned from `internal/daemon/run.go`).
  Each tick diffs the current local capture store against the reference
  snapshot and appends the structured outcome to
  `<drift_log_dir>/<upstream>.jsonl`. Divergence is logged at warn under the
  `providers.mitm.wire` concern; a single upstream's infrastructure failure
  (missing reference, empty transcript) is logged and does not stop the loop.

Operators enable the loop by adding a `[mitm.drift]` block to
`~/.config/clyde/config.toml`. With `enabled = false` (or the block absent),
both the debounced refresh and the periodic runner return immediately and
nothing runs. See `clyde.example.toml` for a populated block. The config shape
lives in `internal/config/mitm_config.go` (`MITMDriftConfig`).

## Seed a baseline manually

The daemon normally creates baselines from live captures. When you already have
a capture transcript and want to seed a v2 baseline without waiting for the
refresher, run:

```
clyde mitm seed-baseline --upstream claude-code --from <transcript.jsonl>
```

This extracts a v2 wire baseline from the transcript and writes
`reference-v2.toml` under the default baseline root for the upstream
(`<XDG_STATE_HOME>/clyde/mitm-baselines/<upstream>/reference-v2.toml`). The
provider filter is derived from the upstream name. Pass `--output` to write the
baseline elsewhere (the file must be named `reference-v2.toml`), and use
`--include-ua` / `--exclude-ua` to scope which captured caller flavor seeds the
baseline (for example `--include-ua claude-cli` to keep only the upstream CLI
and drop the adapter or other clients sharing the proxy).
