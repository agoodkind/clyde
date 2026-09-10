# Daemon Metrics History

`clyde daemon status` reports current daemon state and retained activity summaries.
It reads the running daemon's semantic settings, connection and retry state,
listener addresses, and profiling address. These reads do not start discovery,
connect to the semantic engine, or enable profiling. A disabled semantic operation
differs from an enabled operation whose engine is unavailable.

The metrics worker reads newly appended complete log records and keeps unfinished
requests in memory between passes. It follows an ordinary rotation through its
held source file, then reads the new active file. Unchanged passes skip old log
bytes, and retention rewrites run only when a summary expires. A compatible
saved position resumes the active source after restart; otherwise collection
starts at the active end and reports missing historical summary coverage.
Previously completed summaries remain readable. Unfinished in-memory requests
may be lost when the daemon stops.

`clyde daemon status --since 1h` explicitly reads retained raw daemon logs for
that window. This historical read has a separate cost from ordinary status.

The report shows counters as current, delta, and rate. Counters include request outcomes, bytes, tokens, cache tokens, and estimated cost. Gauges show current, minimum, mean, and maximum for inflight and streaming work. Duration summaries show total, calls, mean, p50, p95, and maximum.

The time breakdown ranks exclusive request stages. Clyde records lifecycle legs as elapsed time from request start. The report subtracts adjacent milestones per request, so nested cumulative durations never inflate totals. Time not covered by an exclusive stage becomes unattributed duration.

Coverage is complete only when retained records reach the window start and a live provider snapshot reaches its end. A restart, malformed relevant record, terminal request without a retained start, read failure, or corrupt rotation makes coverage incomplete. A request that remains active at the window end is normal.

The explicit historical report reads only the daemon process log and its retained
rotations. It continues after a corrupt compressed rotation and records a
coverage warning. Retention controls how far back a report can be complete.

Unknown model pricing makes the historical cost delta and rate unavailable. Other historical counters remain available when their log records are valid.

Use the terminal report for a quick operational check. Use `clyde --output-format json daemon status --since 1h` for machine processing. Its top level contains only `window`, `coverage`, `metrics`, `time_breakdown`, `unattributed_duration_ms`, and `warnings`.
