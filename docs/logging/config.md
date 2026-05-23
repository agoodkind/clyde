# Logging Config

Logging configuration is centralized under `[logging]`.

Payload controls:

- `logging.raw_capture.enabled` controls whether raw payload sidecar files may be written.
- Normal JSONL request logs always use the fixed filtered inline payload view.
- There is no user-facing payload mode ladder.

Cleanup controls:

- `logging.cleanup.enabled` controls whether cleanup deletes eligible aged logs.
- Retention thresholds live in `logging.cleanup.max_age_days`, `logging.cleanup.max_backups`, and `logging.cleanup.max_total_mb`.

Sink controls:

- `logging.sinks.enabled` is the allowlist of stable sink names to enable.
- `logging.concerns` maps registered concern names to concern-specific overrides.

Rotation controls:

- `logging.rotation` configures active file rotation.
- Per-sink rotation overrides may specialize a sink when needed.

Inventory controls:

- `clyde logs inventory --json` returns the default typed discovery view from configured active locations and the `inventory_index` sink.
- `clyde logs inventory --deep --json` performs an exact filesystem scan.

Legacy payload controls are not user-facing logging controls and do not change the shape of normal request logs.
