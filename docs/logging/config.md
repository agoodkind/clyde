# Logging Config

Logging configuration is centralized under `[logging]`.

The `[mitm.capture_store]` table controls exchange recording and retention.

Capture settings:

| Setting | Default | Behavior |
|---|---|---|
| `record_until` | Empty | Enables full exchange recording until this RFC3339 timestamp. An empty or expired value disables recording. |
| `retention_max_age` | `168h` | Deletes older exchanges and drift history. Zero disables age retention. |
| `retention_max_bytes` | `5000000000` | Limits the combined database, WAL, and SHM file sizes. Zero disables size retention. |
| `retention_interval` | `1s` | Controls the maintenance interval. |

The recording deadline applies to MITM HTTP and websocket exchanges, adapter egress, and optional adapter ingress. The store checks the deadline when an exchange completes. An exchange that completes after expiration does not persist. Reloading preserves the absolute deadline. Disabled recording does not disable proxying, injection hooks, baseline learning, or retention of existing captures.

At least one retention limit must stay enabled. Limits cannot be negative, and the maintenance interval must be positive. Maintenance deletes exchanges with their raw bodies, decoded bodies, and decoded tool events in one transaction. It also removes old drift shapes, drift checks, and baseline differences. Current baselines remain available.

The first daemon maintenance run deletes captures that existed when the store opened. SQLite stores the cutoff and completion state for restart recovery. This cleanup preserves captures that the daemon records after opening the store.

Maintenance checkpoints the WAL and reclaims free pages before deleting more retained history. Active readers can delay reclamation. Existing databases that require a full vacuum defer that conversion. Maintenance reports excess bytes, pinned readers, and stalled reclamation in structured logs.

Cleanup controls:

- `logging.cleanup.enabled` controls archive deletion and active-file compaction.
- `logging.cleanup.max_total_mb` limits the combined size of all Clyde-owned log surfaces, including active files and archives.
- `logging.cleanup.max_age_days` and `logging.cleanup.max_backups` limit retained history. Zero disables the corresponding threshold.

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
