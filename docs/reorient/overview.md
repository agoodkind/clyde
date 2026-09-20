# Reorient Delivery Overview

Reorient delivery restores a conversation's pre-compaction transcript after a `/compact`, so the model keeps the detail the compaction summary drops. Tier 1 emits a paging note for Claude Code, Codex, and Cursor. Tier 2 injects transcript content into Claude MITM summaries and authenticated native Codex Responses summaries.

Observed behavior of the current Claude Code client is that a large SessionStart `additionalContext` hook output is spilled to a file and only a short preview is injected, so the model never receives a full transcript delivered that way (this is client behavior, not defined in this repo). Both tiers work around that limit through a channel the client does not spill.

## Tier 1: paging note (no MITM)

The hook emits a small note, under the client's hook-output size limit, that tells the model to page its pre-compaction transcript in through the clyde reorient tool. The note includes the conversation selector from the hook input. Claude Code and Codex emit it from `runReorientAfterCompact` on the post-compact SessionStart event. Cursor has no post-compact event, so `runReorientAfterCompact` no-ops for it and the same note is emitted from `runReorientStopFollowup` on the stop event, but only when a pre-compact snapshot exists for the conversation (`SnapshotStore.Consume` returns ok); a stop event with no snapshot emits nothing. Both hooks live in `internal/hookspec/runner.go`.

## Tier 2: summary injection (opt-in)

When `reorient_summary_injection` is on, the Claude MITM path injects the recovered transcript into the `/compact` summary response. The client persists that content in the `isCompactSummary` user message and sends it on later turns. The transcript is inserted inside the model's `<summary>` span. A response without a closing span receives a trailing block instead.

Detection requires Claude Code's declaration that a request is a compaction turn. Claude Code sets `x-claude-code-request-class: compaction` on that request. It sets `main`, `auxiliary`, or `subagent` on every other request. A survey of the MITM capture store found the compaction class on 1 of 1,173 `/v1/messages` requests, and that request was a deliberately triggered compaction.

A manual `/compact` and an automatic compaction set the same request class. They differ in the separate `x-claude-code-compaction` header, which reads `manual` for one and `reactive` for the other. Detection ignores that header, because the two values are not a shared enum.

`internal/providers/claude/mitmcontrib` converts the class header into `mitm.RequestPurposeCompaction`. The MITM hook seam asks the claiming provider for that value and stores it on the hook request. `internal/reorientinject` reads the stored purpose and reads no Claude header.

The compaction prompt substring remains a secondary condition on the request body. An exported transcript of a session that once compacted reproduces the prompt verbatim. Pasting such a transcript puts the substring in the last user message of an ordinary turn. The same capture-store survey found 60 ordinary requests containing the substring and no compaction among them. In the one real compaction the substring began 379 bytes into the last user message, and a prefix test fails on that request.

Correlation reads the Claude session id from the request's `metadata.user_id` field, a double-encoded JSON string (a JSON string value that itself contains an encoded JSON object). Its `session_id` field matches the `x-claude-code-session-id` header and the `PreCompact` hook payload. The split records that id on the parsed request and opens no file.

### Selecting the retained content by token budget

Every byte the Claude split reads comes from the intercepted request. No compaction path opens a transcript file.

The split counts one content block at a time from the newest message toward the oldest, retains every block that fits the budget whole, and retains the tail of the first block that does not fit. The retained text measures strictly under the budget: it never equals the budget and never exceeds it. The split never retains message index 0, which the model always summarizes.

`internal/adapter/anthropic` implements the per-block decode and truncation. `internal/reorientinject` runs the count.

The counter is `tokencount.LocalCounter` under the `[export]` settings, which is the counter `clyde conversation export --max-tokens` uses. The exact count endpoint stays unused. It is a network call on the request path, and every compaction would wait on it.

The budget comes from a `--max-tokens` argument typed after `/compact`. Claude Code appends that text to the compaction prompt below a line reading `Additional Instructions:`. `internal/providers/claude/compaction` reads the declared `clyde conversation export` arguments from that block and stops at the first token that is not one. Free prose and PreCompact hook instructions reach the model unchanged. Without a `--max-tokens` argument, the budget is `reorient_inject_max_tokens`. An unreadable argument never fails a compaction: the split logs it and uses the configured budget.

### Keeping the forwarded request valid

The forwarded request retains every message before the cut, the boundary message truncated at the cut, and the instruction region unchanged. Anthropic accepts a truncated request only when three structural conditions hold. `internal/adapter/anthropic/compaction_truncate.go` enforces each condition on the rewrite. `compaction_validity_test.go` asserts each condition at every cut of one conversation.

A `tool_use` block's `input` field must remain a JSON object. The rewrite drops whole `input` keys first, then truncates the last remaining string value, keeping the field a JSON object.

Every retained tool call must have a matching `tool_result`. The rewrite appends one synthetic result per unanswered call, marked `is_error`, matching what Claude Code emits for the same case.

A `system` message must precede an `assistant` message or end the array, and a `tool_result` block's matching `tool_use` must be the immediately previous message. The rewrite drops system reminders left last in the kept prefix, and drops a `tool_result` the previous message does not call.

Every failure forwards the request unmodified and leaves the response unchanged. A decode, parse, count, truncate, or inject failure logs `mitm.reorient_inject.split_fallback` with a reason. A planned split logs `mitm.reorient_inject.split_planned` with the message index, the segment index, the head runes, the budget, and the retained token count.

### Native Codex Responses

The authenticated native Codex path splits by unit count, not by token budget.
It reads `reorient_recent_fraction`, `reorient_context_window_fraction`,
`reorient_bytes_per_token`, and `reorient_standard_context_window`, and
`reorient_inject_max_tokens` caps the result. The Claude split reads none of the
first four.

The `responses` implementation keeps the final synthesized user item
byte-for-byte, splits the earlier request input on complete turn and tool-pair
boundaries, and appends the removed transcript to the final assistant summary.
It reads no disk fallback.

The `responses_compaction_v2` implementation keeps turn N's encrypted
compaction response byte-identical. Clyde stores the bounded selected transcript
in process memory after that successful response. On the matching regular turn
N+1, it inserts one tagged assistant history item after the encrypted item in
the upstream request. It appends that tag once to the first successful regular
final assistant response. Codex then resends the tagged result naturally on
N+2, so Clyde clears the pending entry after the N+1 append succeeds.

The v2 entry expires after two hours, stores at most 32 pending recoveries, and
does not survive a daemon restart. Expiry, restart, correlation, parsing, or
response transformation failure forwards native v2 traffic unchanged. Codex
uses this path only through its built-in `openai` provider when
`openai_base_url` points at Clyde.

## Hook seam

Tier 2 rides an in-process MITM request/response hook seam in `internal/mitm/response_hook.go`, registered per proxy through `SetRequestResponseHooks` and attached at the shared forward paths so one registration covers every listener. The seam decodes a matched response body (gzip, deflate, zstd) before the transformer rewrites it, and forwards an undecodable encoding untouched so a compressed body is never handed back mislabeled. `internal/mitm/response_hook_test.go` and `internal/mitm/response_hook_decode_test.go` hold the behavior. The daemon registers the reorient hook in `internal/daemon/runtime.go` only when the flag is on (`internal/daemon/reorient_inject_wiring_test.go`).
