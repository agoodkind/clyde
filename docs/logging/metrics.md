# Daemon Metrics History

`clyde daemon status --since 1h` reads Clyde's retained daemon JSONL log and reports adapter activity for that window. Plain `clyde daemon status` keeps the supervisor and worker report.

The report shows counters as current, delta, and rate. Counters include request outcomes, bytes, tokens, cache tokens, and estimated cost. Gauges show current, minimum, mean, and maximum for inflight and streaming work. Duration summaries show total, calls, mean, p50, p95, and maximum.

The time breakdown ranks exclusive request stages. Clyde records lifecycle legs as elapsed time from request start. The report subtracts adjacent milestones per request, so nested cumulative durations never inflate totals. Time not covered by an exclusive stage becomes unattributed duration.

Coverage is complete only when retained records reach the window start and a live provider snapshot reaches its end. A restart, malformed relevant record, terminal request without a retained start, read failure, or corrupt rotation makes coverage incomplete. A request that remains active at the window end is normal.

The report reads only `clyde-daemon.jsonl` and its retained rotations. It continues after a corrupt compressed rotation and records a coverage warning. Retention controls how far back a report can be complete.

Unknown model pricing makes the historical cost delta and rate unavailable. Other historical counters remain available when their log records are valid.

Use the terminal report for a quick operational check. Use `clyde --output-format json daemon status --since 1h` for machine processing. Its top level contains only `window`, `coverage`, `metrics`, `time_breakdown`, `unattributed_duration_ms`, and `warnings`.
