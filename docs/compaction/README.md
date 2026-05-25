# Compaction hub

Compaction is the clyde subsystem that shortens an active session's transcript so the next time the user resumes the session, claude reads a smaller history and has more headroom inside its context window. The planner reads the on-disk JSONL, runs a four-stage pipeline of Rehydrate, axis bisection, Dehydrate, and Apply, and writes one new `compact_boundary` plus one new synthetic-summary entry that absorbs whatever the planner dropped. The session resumes from the same JSONL file: nothing is deleted, the boundary just moves invisibly forward.

This hub is the entry point for working on compaction. The pages below cover the moving parts, the shared vocabulary, and the procedures the smoke runbook uses to verify changes.

## Pages

- [algorithm.md](algorithm.md) describes the pipeline end-to-end: the inputs the planner takes, the four axes it runs in fixed order, the Rehydrate and Dehydrate passes that flank the bisect, the Bisect Axis primitive, and the projection arithmetic.
- [glossary.md](glossary.md) is the shared vocabulary the other pages assume. Read this first if a term in any other page is unfamiliar.
- [edge-cases.md](edge-cases.md) catalogs the failure modes the pipeline has to tolerate, including legacy synthetic shapes, a target below the floor, probe `WorkDir` mismatch, and attached-session probe pollution.
- [canary-system.md](canary-system.md) documents the canary methodology the smoke runbook uses to verify compaction correctness, including the metadata-footer canary form, the prompt-injection trap, and the spawn-flag rule.
- [acceptance.md](acceptance.md) is the pointer into the smoke runbook for end-to-end verification, including the pass-and-fail matrix.
- [providers/claude.md](providers/claude.md) covers the Claude provider's probe spawn shape, the `/context` slash command argv, the `result` envelope and markdown table parser, and the `ProbeOptions.WorkDir` requirement.
- [providers/codex.md](providers/codex.md) is the stub for the Codex provider's pending compaction implementation.

## External references

- [docs/compaction-smoke-runbook.md](../compaction-smoke-runbook.md) is the executable runbook that exercises the entire compaction surface against a real session. The acceptance matrix at the end of the runbook is the canonical pass-and-fail oracle for compaction changes.
- [docs/SLOG.md](../SLOG.md) is the structured-logging contract the planner and the probes emit against. Every event named in this hub (`compact.probe.completed`, `compact.candidate.probe_started`, `compact.synthetic_summary.parse_skipped`, and so on) follows the conventions documented there.
