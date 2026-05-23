# Log Inventory

`clyde logs inventory` is the first-class discovery surface for active log locations.

The JSON output includes:

- `state_root`
- `generated`
- `mode`
- `raw_capture_enabled`
- `cleanup_enabled`
- `categories`

Each category includes:

- `category`
- `sink`
- `source`
- `count`
- `total_bytes`
- `latest_modified`
- `representative_path`
- `last_event_timestamp`
- `last_event_request_id`
- `last_cleanup_result`
- `raw_capture_enabled`
- `cleanup_enabled`
- `rotation`
- `cleanup`
- `largest_files`

`last_event_timestamp` and `last_event_request_id` reflect the most recent event the sink received, as recorded by the lightweight `inventory_index` sink at `logs/inventory/events.jsonl`. `last_cleanup_result` is the typed `slogger.cleanup.completed` payload (scanned roots, candidate count, deleted count, bytes deleted, skipped paths, errors, duration) for the most recent cleanup pass, repeated on every cleanup-eligible category and omitted on lock files and uncategorized entries.

Use the default indexed mode for routine diagnostics. Indexed mode stats configured active log locations and parses the lightweight `inventory_index` sink instead of recursively walking the full state tree.

Deep mode is a pure filesystem walk. `clyde logs inventory --deep` does not read `logs/inventory/events.jsonl`, so fields derived from the inventory index (`last_event_timestamp`, `last_event_request_id`, `last_cleanup_result`) remain empty in deep mode even when the index file exists. Use `--deep` when exact file counts matter, such as manual deletion of aged logs or verification of a suspected stale index.

Inventory categories should cover process logs, concern logs, per-chat logs, inventory indexes, MITM capture indexes, MITM raw sidecars, provider sidecars, and LaunchAgent fallback logs. Add a category only when a stable sink or external fallback location appears.
