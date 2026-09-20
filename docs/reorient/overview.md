# Reorient Delivery Overview

Reorient delivery restores a conversation's pre-compaction transcript after a `/compact`, so the model keeps the detail the compaction summary drops. Tier 1 emits a paging note for Claude Code, Codex, and Cursor. Tier 2 injects transcript content into Claude MITM summaries and authenticated native Codex Responses summaries.

Observed behavior of the current Claude Code client is that a large SessionStart `additionalContext` hook output is spilled to a file and only a short preview is injected, so the model never receives a full transcript delivered that way (this is client behavior, not defined in this repo). Both tiers work around that limit through a channel the client does not spill.

## Tier 1: paging note (no MITM)

The hook emits a small note, under the client's hook-output size limit, that tells the model to page its pre-compaction transcript in through the clyde reorient tool. The note carries the conversation selector from the hook input. Claude Code and Codex emit it from `runReorientAfterCompact` on the post-compact SessionStart event. Cursor has no post-compact event, so `runReorientAfterCompact` no-ops for it and the same note is emitted from `runReorientStopFollowup` on the stop event, but only when a pre-compact snapshot exists for the conversation (`SnapshotStore.Consume` returns ok); a stop event with no snapshot emits nothing. Both hooks live in `internal/hookspec/runner.go`. `internal/hookspec/runner_test.go` and `internal/hookspec/output_snapshot_test.go` hold the behavior.

## Tier 2: summary injection (opt-in)

When `reorient_summary_injection` is on, the Claude MITM path injects the recovered transcript into the `/compact` summary response. The client persists that content in the `isCompactSummary` user message and sends it on later turns. The transcript is inserted inside the model's `<summary>` span. A response without a closing span receives a trailing block instead.

Detection requires Claude Code's declaration that a request is a compaction turn. Claude Code sets `x-claude-code-request-class: compaction` on that request. It sets `main`, `auxiliary`, or `subagent` on every other request. A survey of the MITM capture store found the compaction class on 1 of 1,173 `/v1/messages` requests, and that request was a deliberately triggered compaction.

A manual `/compact` and an automatic compaction set the same request class. They differ in the separate `x-claude-code-compaction` header, which reads `manual` for one and `reactive` for the other. Detection ignores that header, because the two values are not a shared enum.

`internal/providers/claude/mitmcontrib` converts the class header into `mitm.RequestPurposeCompaction`. The MITM hook seam asks the claiming provider for that value and stores it on the hook request. `internal/reorientinject` reads the stored purpose and reads no Claude header.

The compaction prompt substring remains a secondary condition on the request body. An exported transcript of a session that once compacted reproduces the prompt verbatim. Pasting such a transcript puts the substring in the last user message of an ordinary turn. The same capture-store survey found 60 ordinary requests containing the substring and no compaction among them. In the one real compaction the substring began 379 bytes into the last user message, and a prefix test fails on that request.

Correlation reads the Claude session id from the request's `metadata.user_id` field, a double-encoded JSON string (a JSON string value that itself contains an encoded JSON object). Its `session_id` field matches the `x-claude-code-session-id` header and the `PreCompact` hook payload, and it is the basename of the on-disk transcript file. `internal/providers/claude/parser/session_path.go` resolves that session id to a path without the index (`internal/providers/claude/parser/session_path_test.go`).

Content comes from `Index.RenderReorientArtifact` in `internal/conversation/reorient.go`, which builds the record directly from the transcript path and renders it with the reorient knobs, so it avoids the index resolve and refresh path. `internal/conversation/reorient_artifact_test.go` holds the behavior.

The Claude hook splits a large compaction request. The model summarizes the older half, and the hook injects the recent half. The hook removes the recent half and every older-half `system` message from the forwarded request, because Claude Code sends harness reminders as back-to-back `system` messages that the API rejects once the request is trimmed. The injected recent half is the transcript parser's render, sized to the removed half's bytes with no line cap, so harness reminders are stripped the same way as in every other export. The hook trims only after that render succeeds. A request with no valid split, an invalid trim, or an empty render goes upstream whole and receives the line-capped render instead, and the hook logs `mitm.reorient_inject.split_fallback` with the reason.

The authenticated native Codex path uses the same reorient sizing controls. The
`responses` implementation keeps the final synthesized user item byte-for-byte,
splits the earlier request input on complete turn and tool-pair boundaries, and
appends the removed transcript to the final assistant summary. It reads no disk
fallback.

The `responses_compaction_v2` implementation keeps turn N's encrypted
compaction response byte-identical. Clyde holds the bounded selected transcript
in process memory after that successful response. On the matching regular turn
N+1, it inserts one tagged assistant history item after the encrypted item in
the upstream request. It appends that tag once to the first successful regular
final assistant response. Codex then resends the tagged result naturally on
N+2, so Clyde clears the pending entry after the N+1 append succeeds.

The v2 entry expires after two hours, holds at most 32 pending recoveries, and
does not survive a daemon restart. Expiry, restart, correlation, parsing, or
response transformation failure forwards native v2 traffic unchanged. Codex
uses this path only through its built-in `openai` provider when
`openai_base_url` points at Clyde.

## Hook seam

Tier 2 rides an in-process MITM request/response hook seam in `internal/mitm/response_hook.go`, registered per proxy through `SetRequestResponseHooks` and attached at the shared forward paths so one registration covers every listener. The seam decodes a matched response body (gzip, deflate, zstd) before the transformer rewrites it, and forwards an undecodable encoding untouched so a compressed body is never handed back mislabeled. `internal/mitm/response_hook_test.go` and `internal/mitm/response_hook_decode_test.go` hold the behavior. The daemon registers the reorient hook in `internal/daemon/runtime.go` only when the flag is on (`internal/daemon/reorient_inject_wiring_test.go`).
