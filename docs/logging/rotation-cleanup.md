# Rotation And Cleanup

Clyde keeps file rotation and cleanup separate.

Rotation controls active log files. It applies max file size, max backups, max age, and compression policy. Rotation is driven by `gklog` `RotationConfig`, with the rotation policy declared in `internal/logpolicy` and `internal/config`, while `internal/slogger` converts that policy into the `gklog` config and owns the cleanup walker.

Cleanup controls stale files. It is enabled by `logging.cleanup.enabled` and uses retention settings to remove aged rotated files, aged sidecars, empty artifacts, and files that exceed total budget rules.

When cleanup is disabled, Clyde may audit eligible files, but it should not delete them.

Cleanup applies across the logging surfaces exposed by inventory, aligned to the inventory categories in `internal/cli/logs/inventory.go`:

- MITM raw captures.
- MITM profile/process logs.
- Top-level daemon/cli logs.
- Provider sidecar logs.
- Concern logs.
- Per-chat transcript logs.
- Inventory indexes.
- Pre-repair retained logs.
- Lock files.
- Uncategorized.

MITM wire legs are concern logs at `logs/providers/mitm/wire.jsonl`, so they fall under Concern logs.

Use `clyde logs inventory --deep --json` around manual cleanup when exact file counts matter.

## Cleanup and inventory wiring

```mermaid
flowchart LR
  config["logging.cleanup\nconfig"] --> applyCleanupPolicy["applyCleanupPolicy"]
  applyCleanupPolicy --> walk["filepath.WalkDir(state_root)"]
  walk --> candidates["candidates: jsonl, log, raw"]
  candidates --> remove["os.Remove(candidate)"]
  remove --> cleanupCompleted["slogger.cleanup.completed event"]
  cleanupCompleted --> sinkInventory["sink: inventory_index"]
  sinkInventory --> eventsJSONL["logs/inventory/events.jsonl"]
  inventoryCLI["clyde logs inventory"] --> indexedMode["indexed mode"]
  indexedMode --> eventsJSONL
  indexedMode --> statKnown["os.Stat(known paths)"]
  inventoryCLI --> deepMode["deep mode"]
  deepMode --> walk
```

The cleanup pass emits `slogger.cleanup.completed` with `scanned_roots`, `candidates`, `deleted`, `bytes_deleted`, `skipped`, `errors`, and `duration_ms`. The event flows through the inventory sink to `logs/inventory/events.jsonl`, which `clyde logs inventory` reads in indexed mode to surface the most recent cleanup result per sink. Deep mode stays off that snapshot path and reports only filesystem-derived totals.
