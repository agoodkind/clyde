# Claude provider: compaction internals

The Claude provider implements the live `/context` probe the compaction planner depends on. The probe lives in `internal/providers/claude/contextusage/` because it carries Claude-specific spawn shape and output parsing; the generic `internal/contextusage/` package stays provider-neutral.

See [../algorithm.md](../algorithm.md) for how the planner consumes the probe and [../edge-cases.md](../edge-cases.md) for the probe failure modes.

## The `/context` probe shape

`ProbeContextUsage` in `probe_spawn.go` runs the `claude` binary in print mode with the `/context` slash command. The probe reads claude's stdout as a single JSON envelope, extracts the markdown table embedded in the `result` field, and parses the categories and totals into a Snapshot.

The argv is fixed:

```
-p /context --resume <session-id> --model <model>
--no-session-persistence --output-format json --max-turns 1
```

`-p /context` runs the slash command as a one-shot prompt. `--resume <session-id>` loads the target session's transcript so the Snapshot reflects that session's MCP tool surface, memory files, custom system prompt, and message history. `--model <model>` pins the model so the Snapshot's `maxTokens` matches the model the planner is targeting. `--no-session-persistence` keeps the probe from writing session-history side effects to disk. `--output-format json` wraps stdout as a JSON list of typed envelopes, including the `result` entry carrying the markdown. `--max-turns 1` bounds the call to a single non-model slash-command turn.

`cmd.Dir` is set from `ProbeOptions.WorkDir`. claude resolves memory files (`CLAUDE.md`), skills, agents, and MCP servers from the current working directory, so the cwd has to match the directory the live session uses.

## The `result` envelope and the markdown table

claude emits a sequence of stream events on stdout, the last of which is a `result` envelope:

```json
{"type": "result", "subtype": "success", "result": "## Context Usage\n\n**Model:** ...\n\n### Estimated usage by category\n\n| Category | Tokens | Percentage |\n|----------|--------|------------|\n| System prompt | 6.3k | 3.1% |\n...", "session_id": "..."}
```

`scanForUsage` in `probe_spawn.go` walks stdout line by line until it finds the `result` envelope, then parses the markdown:

- `**Tokens:** N / M (P%)` gives `TotalTokens`, `MaxTokens`, `Percentage`.
- `**Model:** <name>` gives `Model`.
- Each row of the category table (`| Category | Tokens | Percentage |`) becomes one `Snapshot.Category`. The "Tokens" cell is a short form (`6.3k`, `1.6k`, `< 20`, plain integer); the parser multiplies the `k`/`m` suffixes, accepts the `< N` form as `N`, and rejects empty rows.

The parser ignores the per-tool MCP table, the memory files table, and the skills table. Only the category totals feed the Snapshot the planner reads.

## `ProbeOptions.WorkDir`

The probe sets `cmd.Dir` from `ProbeOptions.WorkDir`. The directory has to match the directory the original interactive session ran from, because claude resolves memory files, skills, agents, and MCP servers from its current working directory. A probe spawned from the wrong cwd loads a different set of these inputs than the live session uses, so its `/context` categories diverge from reality.

The mismatch produces planner errors that the user feels as inconsistency rather than as a hard failure. The planner may commit a result that drops too much or too little relative to what the live session would, because the probe's category totals diverge from reality. The planner emits `work_dir` on every probe log so the divergence is recoverable from logs after the fact.

## What the Snapshot reflects

The categories the probe returns describe the session at the moment of the call, with one caveat: MCP servers warm up incrementally. A session that has just connected to its MCP servers reports fewer "MCP tools" tokens than the same session a minute later once every server has declared its full tool list. Calls during warm-up undercount the static overhead. The planner's `compact.probe.completed` log line includes the snapshot's `total_tokens` and `model` so the developer can spot a warm-up artifact when investigating.
