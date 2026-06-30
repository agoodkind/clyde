# Logging Layout

Log layout is discovered through `clyde logs inventory`. The rules here describe stable classes only, not local machine paths.

Stable classes, in inventory `categoryOrder`:

- MITM request/response bodies persist to the SQLite capture store at `mitm/capture.db`, not to a log category.
- MITM profile/process logs hold MITM launcher, profile, and process-monitor output.
- Top-level daemon/cli logs hold daemon and CLI process events.
- Provider sidecar logs hold provider-specific safe summaries and transport diagnostics.
- Concern logs hold concern-routed JSONL under the active concern root. Captured forward-proxy MITM wire legs are concern-routed here under the `providers.mitm.wire` concern, at `logs/providers/mitm/wire.jsonl`.
- Per-chat transcript logs hold unified request events for one chat key.
- Inventory indexes hold lightweight discovery data for default inventory mode under the `inventory_index` sink.
- Pre-repair retained logs hold files preserved by a log-repair pass.
- Lock files hold flock and lock-file entries.
- Uncategorized logs hold anything that matches no other class. The macOS LaunchAgent fallback file `daemon.log` currently classifies as Uncategorized logs.

File naming rules:

- JSONL logs use `.jsonl` suffixes.
- Per-chat files use a sanitized chat key and must stay under the configured chat log root.
- Rotated files use the cleanup class of their active file.

Humans and LLMs should start with `clyde logs inventory --json` instead of guessing paths. Use `--deep` when the exact filesystem view matters.
