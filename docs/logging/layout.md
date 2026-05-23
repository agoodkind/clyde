# Logging Layout

Log layout is discovered through `clyde logs inventory`. The rules here describe stable classes only, not local machine paths.

Stable classes:

- Process logs hold daemon, TUI, and CLI process events.
- Concern logs hold concern-routed JSONL under the active concern root.
- Per-chat logs hold unified request events for one chat key.
- MITM capture indexes hold typed capture records for captured forward-proxy traffic.
- MITM raw sidecars hold raw request or response bytes only when `logging.raw_capture.enabled` is true.
- Provider sidecars hold provider-specific safe summaries and transport diagnostics.
- Inventory indexes hold lightweight discovery data for default inventory mode under the `inventory_index` sink.
- LaunchAgent fallback logs hold macOS process output when the service manager captures it externally.

File naming rules:

- JSONL logs use `.jsonl` suffixes.
- Per-chat files use a sanitized chat key and must stay under the configured chat log root.
- MITM raw sidecars stay under the configured raw capture root and are referenced from the typed MITM facet.
- Rotated files remain in the same cleanup class as their active file.

Humans and LLMs should start with `clyde logs inventory --json` instead of guessing paths. Use `--deep` when the exact filesystem view matters.
