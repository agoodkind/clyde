# Logging inventory

The inventory index records where logs live so the CLI can enumerate them. Records and categories are emitted by `internal/cli/logs`.

## Record fields

Each inventory record carries `path`, `category`, `sizeBytes`, and `modTime`.

## Categories

The `inventoryCategory` constants are, in order: `mitm_capture_index`, `mitm_profile`, `daemon_cli`, `provider_sidecar`, `concern`, `per_chat`, `inventory_index`, `pre_repair`, `lock`, and `uncategorized`.

MITM wire legs route through the `providers.mitm.wire` concern to `logs/providers/mitm/wire.jsonl`, surfaced under the `concern` category. Full MITM request/response bodies persist to the SQLite capture store at `mitm/capture.db`, which is not a log file and is not inventoried. Add a category only when a stable sink or external fallback location appears.
