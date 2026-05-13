# Compaction fix chain, closed record 2026-05-12

This file records the resolution of the compaction bug chain that opened on 2026-05-11. Delete this file once the documentation hub in `docs/compaction/` ships.

## What was broken

A targeted compaction Apply against an attached chat session silently nuked the recent chat content. The root cause was a soft-fail in `BuildRuntimeUpfront`: when the `/context` probe failed, the function continued with a zero-token snapshot, the planner ran one iteration on phantom numbers, the run completed `hit_target=True`, and Apply mutated the transcript behind a near-empty synthetic. The probe itself failed because the daemon spawned `claude --resume` with the launchd cwd (`/`) instead of the session's project directory, and claude exits with a single stdout line and no `control_response` when it cannot orient on the project root.

## What shipped

The `internal/contextusage.ProbeOptions` contract gained a `WorkDir` field. Every call site that constructs a Prober probe now populates `WorkDir` from `sess.Metadata.WorkspaceRoot`. The claude adapter forwards `WorkDir` into `cmd.Dir` on the spawn so the subprocess anchors at the project root. `BuildRuntimeUpfront` now returns an error when the upfront probe fails and a non-zero target is set, so a future regression on the probe cannot silently produce a phantom Apply. Fix 4 (refuse Apply when projection exceeds target) remains the safety net for the case where the planner cannot reach the target even with maximum drops.

The planner's chat-drop loop was rewritten to use a generic bisect primitive (`internal/compact/bisect.go`). Convergence dropped from one probe per dropped turn to roughly `log2(N)+1` probes. The smoke runbook against motd shows planning that previously took three minutes now completes in twenty to thirty seconds.

The earlier acceptance fixes shipped in their own commits: Undo now restores from the gzipped snapshot rather than truncating by recorded offset, so a mid-file mutation between Apply and Undo no longer corrupts the tail; Undo deletes the snapshot file after a successful restore; the planner's per-iteration counter now reads `/context` through the Prober rather than Anthropic's `count_tokens` HTTP API.

## Acceptance status

The full smoke runbook runs cleanly against the `motd-shell-rules-cleanup` session at full chain. Pre-Apply probe returns the survivor UUID, Apply at a feasible target succeeds in roughly seven seconds, Apply at an infeasible target refuses with the projection-over-target message, the post-Apply probe still returns the survivor UUID, the boundary canary spliced into pre-boundary content is correctly hidden from the model, Undo produces a byte-identical pre-Apply transcript, and the pristine restore from the `/tmp` backup matches the expected sha256.

## Closed tickets

The closure set is CLYDE-345, CLYDE-356, CLYDE-373, CLYDE-374, CLYDE-375, CLYDE-378, CLYDE-383, CLYDE-388, CLYDE-389, and CLYDE-393.

## Pending follow-ups

The Rehydrate and Dehydrate boundary that lets the planner stop knowing synthetics exist is in progress as CLYDE-386 with five phase children CLYDE-413 through CLYDE-417. Phase A (Rehydrate pass-through) shipped in commit `418b425`. Phase B (Dehydrate pass-through) shipped in commit `d490d25`. The real decomposition, recomposition, and planner cleanup remain. The liveruntime extraction that moves provider-specific live-session code out of the daemon is filed as CLYDE-394 with phase children CLYDE-408 through CLYDE-412. The file-splitting work to bring oversized daemon files under the AGENTS.md line guideline is CLYDE-384 with phase children CLYDE-395 through CLYDE-399. The compaction documentation hub is CLYDE-385 with phase children CLYDE-402 through CLYDE-407. Type hygiene cleanup is CLYDE-392 with phase children CLYDE-400 and CLYDE-401.

The full ordered queue lives in the handoff at `~/.claude/plans/handoff-2026-05-12-evening.md`.
