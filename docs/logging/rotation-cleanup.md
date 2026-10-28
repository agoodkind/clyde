# Log Retention

Clyde applies one combined size budget to its process, concern, provider, chat,
inventory, audit, metrics, and supervisor logs. Each active file can remain below
its rotation threshold while the combined size exceeds this budget.

File rotation limits individual writers. The daemon also runs cleanup after startup
and periodically during operation. CLI startup does not scan log directories.
Cleanup reads the current policy on each pass, including configuration changes.

Cleanup discovers log files under Clyde-owned roots. Configured external file
paths include only the selected file and its recognized archives. Cleanup does
not recursively scan an external file's parent directory, follow symbolic links,
or delete lock files. SQLite capture retention uses a separate policy under
`mitm.capture_store`.

Cleanup removes archives that exceed age or backup limits. When the combined
size still exceeds the budget, cleanup removes older archives first and then
compacts older active files. Compaction keeps complete trailing lines on the
existing inode, which identifies the open file independently of its path. Cached
chat and concern writers continue appending to that file after compaction.

Writers and cleanup use the same per-file lock. Cleanup skips a busy file and
reports the skipped path. A later pass retries retention. The daemon checks the
open supervisor descriptors before cleanup. Cleanup compacts files with append
descriptors in place and protects files with descriptors that do not append.
Protected files still count toward the budget, and cleanup reports any remaining
excess bytes. Supervisor stderr does not acquire the per-file lock. Compaction
of append-mode stderr treats concurrent lines as best effort.

Each compaction reads a bounded tail rather than loading the entire log. Cleanup
counts bytes only after a successful removal or compaction and continues after
individual failures. A failed deletion does not count as freed space.

The `slogger.cleanup.completed` event reports discovered and retained bytes,
deleted and trimmed bytes, changed paths, skipped paths, errors, and duration.
Unchanged passes use debug level. Passes that change files or report errors use
info level. Active writes can temporarily exceed the budget between passes.
