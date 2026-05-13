# Codex provider: compaction internals

The Codex provider does not yet implement the probes the compaction planner depends on. A future pair of slices adds a Codex-specific context probe and a Codex `CandidateProber` under `internal/providers/codex/contextusage/`, mirroring the structure the Claude provider already uses.

The work has two prerequisites that block implementation today. The first is a Codex-side equivalent of Claude's `/context` snapshot: the planner needs a per-session context-usage report that breaks tokens down by category so the Messages delta can drive candidate comparison. The second is a Codex-side way to spawn a disposable candidate session against a synthesized transcript, comparable to how the Claude `CandidateProber` writes a disposable JSONL next to the live session and resumes claude against the disposable id.

Compaction against a Codex session currently fails at the planner's probe stage because no `CandidateProber` is registered under the Codex provider id. The user-facing surface is a typed error rather than a silent miscalculation. See [claude.md](claude.md) for the shape the Codex implementation is expected to take.
