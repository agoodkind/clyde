# Logging Config

Logging configuration is centralized under `[logging]`.

Payload controls:

- Normal JSONL request logs carry no payload view; no log line restates request or response body content, summary, or hash.
- There is no user-facing payload mode ladder.
- Request and response bodies are not a `[logging]` concern; they persist to the SQLite capture store at `mitm/capture.db`, configured under `[mitm.capture_store]`.

Cleanup controls:

- `logging.cleanup.enabled` controls whether cleanup deletes eligible aged logs.
- Retention thresholds live in `logging.cleanup.max_age_days`, `logging.cleanup.max_backups`, and `logging.cleanup.max_total_mb`.

Sink controls:

- `logging.sinks.enabled` is the flat allowlist of stable sink names to enable.
- Per-sink config also uses `[logging.sinks.<name>]` table blocks, each with `enabled`, `level`, `path`, and `rotation`, to specialize an individual sink alongside the flat allowlist.
- The flat allowlist and the per-sink table blocks resolve through one declarative sink registry in `internal/config/logging_config.go`, validated by `IsKnownLoggingSink`.
- The canonical sink names are `daemon`, `cli`, `codex_sidecar`, `anthropic_sidecar`, `audit`, `concerns`, `transcripts`, and `inventory_index`. MITM wire legs route through the `concerns` sink to `logs/providers/mitm/wire.jsonl`; full MITM bodies persist to `mitm/capture.db`.
- `logging.concerns` maps registered concern names to concern-specific overrides.

Rotation controls:

- `logging.rotation` configures active file rotation.
- Per-sink rotation overrides may specialize a sink when needed.

Inventory controls:

- `clyde logs inventory --output-format json` returns the default typed discovery view from configured active locations and the `inventory_index` sink, which backs the indexed inventory view at `logs/inventory/events.jsonl`.
- `clyde logs inventory --deep --output-format json` adds `--deep` to perform an exact filesystem scan.

Legacy payload controls are not user-facing logging controls and do not change the shape of normal request logs.
