# Compaction edge cases

Each case below describes a failure mode the compaction pipeline has to tolerate, the root cause that produces it, and what the user sees when the case fires. See [algorithm.md](algorithm.md) for the normal pipeline and [glossary.md](glossary.md) for vocabulary.

## Legacy synthetic header

A prior synthetic written before the current synthetic-summary format does not decompose into chunks during Rehydrate, so the planner treats the whole synthetic as one opaque drop unit rather than several finer drops at chunk granularity.

The synthetic-summary contract evolved over time. Older synthetics carry a header line of the form `## Continued from prior session (transcript below)`. The `parseSyntheticSummary` function recognizes the legacy header and parses what it can, but legacy synthetics lack the structured chunk keys that current synthetics carry, so the parser cannot break them down into individual drop units.

Compaction still works against a legacy synthetic. The user observes one drop step that removes the entire legacy synthetic, instead of several finer drops that would let the planner keep some chunks and drop others.

## Synthetic parse skipped

A synthetic that the parser cannot read stays opaque to Rehydrate and acts as a single drop unit, the same way a legacy synthetic does.

The parser emits a `compact.synthetic_summary.parse_skipped` debug event with one of two reasons. The `missing_content_array` reason fires when the synthetic stored content as a plain string instead of a content array. The `non_text_block` reason fires when the content array carries blocks the parser does not handle, such as images embedded inside a synthetic. Either condition signals that a code path the planner does not own end-to-end produced the synthetic.

The user sees the same coarser drop resolution as in the legacy header case. A developer investigating the loss of resolution can grep for `parse_skipped` in the daemon log to confirm which synthetic the parser skipped and why.

## Nested synthetics beyond the depth limit

A session that has been compacted more times than `Rehydrate`'s `maxLayers` argument allows leaves the innermost layers unexpanded, so deep history remains an opaque blob the planner cannot trim further.

The `Rehydrate` pass runs at most `maxLayers` iterations, and each iteration expands the first synthetic it finds. The bound exists so a pathological transcript with hundreds of nested synthetics does not cause the planner to spend unbounded time on expansion before the bisect runs.

A session compacted more times than `maxLayers` still compacts, but the deepest layer counts toward the projection as one indivisible chunk. The planner cannot reduce the floor below the size of that chunk plus the static overhead and reserved buffer.

## Target below the floor

The transcript's floor sits above the requested target after every enabled axis has searched to its maximum k. The floor combines the static overhead, the reserved buffer, and whatever post-boundary content cannot be dropped under the bits the caller authorized. No combination of strippers can bring the projection down to the target without dropping content the caller did not authorize.

This is not an error. Per [the target contract](algorithm.md#the-target-contract), the planner commits the smallest projection it can reach, which sits above the target, and a result above the target is valid because the result must be greater than or equal to the target. The planner always commits a result rather than declining to act.

## Probe WorkDir mismatch

A `/context` probe returns numbers that do not match what the live session sees, or the probe fails to resolve memory files and skills entirely, when the probe runs from the wrong working directory.

claude resolves memory files such as `CLAUDE.md`, skills, agents, MCP servers, and other workspace-relative configuration from its current working directory. The `ProbeOptions.WorkDir` field on the spawn options sets that cwd. When `WorkDir` does not match the directory the original interactive session ran from, the probe loads a different set of memory files and skills than the live session uses, so its `/context` categories diverge from reality.

Compaction plans against a wrong baseline when this happens. The planner may then commit a result that drops too much or too little relative to what the live session would, because the probe's category totals diverge from reality. The planner emits `work_dir` on every probe log so the divergence is recoverable from logs after the fact.

## MCP warm-up undercount

A `/context` probe returns fewer "MCP tools" tokens than the live session reports once the same session has been open for some time, when the probe runs immediately after the session connects.

MCP servers declare their tool lists asynchronously as they finish connecting. Each server's contribution to the "MCP tools" category becomes visible to claude only after that server has declared its tools. A probe that fires during warm-up sees the partial set.

The planner sees an undercount of the static overhead floor. The compaction result lands above target by a margin that grows with the number of tools that finished declaring after the probe ran. The probe's `compact.probe.completed` log line carries the snapshot's `total_tokens` and `model` so the developer can spot the warm-up artifact in retrospect.
