> **TRANSIENT DOCUMENT.**
>
> Captured during the 2026-05-11 verification run as short-term evidence for the post-daemon-ownership bug-fix chain. Delete after the fix chain ships and the acceptance matrix in `docs/compaction-smoke-runbook.md` shows all passes. Do not treat as canonical. The durable methodology lives in the runbook. Per-fix acceptance tests live in `~/.claude/plans/post-daemon-ownership-fixes.md`. This document records one specific run, not the design.

# before-chat-only-apply

Test against `main` at commit `a153f76` (pre-refactor). Daemon `cd8b701+dirty` installed at `~/.local/bin/clyde`.

## Cell metadata

| Field | Value |
|-------|-------|
| Cell sentinel | `8D5C0C33-68BD-494D-AC44-EF9B098EB4E8` |
| Start time UTC | `2026-05-12T03:49:51Z` |
| Target session | `lm-review-project-config` (id `8848d3ab-e4ed-4e6b-94c9-903109a3425b`) |
| Variant | chat-only (uncheck thinking, images, tools; leave chat checked) |
| Target tokens | 200000 |
| Test goal | Reproduce or refute CLYDE-345 against the chat-only stripper |

## Pre-flight snapshot (no mutation)

| Field | Value |
|-------|-------|
| Transcript path | `~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl` |
| Pre-flight size | 35,184,415 bytes |
| Pre-flight sha256 | `49a4bae183a8e9521744510d0cfe29a4e7bbe7f7d0f257598be418301cc6f5f4` |
| Pre-flight line count | 17,326 |
| /tmp backup at start | size 35,184,415, sha `49a4bae1...` (byte-identical to live transcript) |
| Existing compact_boundary lines | 3980, 8882, 11362 (most recent at 11362) |
| Post-boundary slice | lines 11363-17326 (5,964 lines) |

## Canary planning

The session is 871k tokens current and the post-boundary slice contains roughly 839k messages tokens. Chat-only at target 200k means the planner must drop about 76% of post-boundary content from the oldest end. The survivor region is approximately the last 1,431 lines (lines 15895-17326).

A first probe attempt revealed an additional behavior. Claude Code on resume loaded only about 456k of the 871k available tokens. The loader dropped the earliest part of the post-boundary slice from the model context. The visibility cutoff lay somewhere between line 11414 (not visible to the model on resume) and line 14971 (visible to the model on resume). This is independent of any Clyde compaction; it is Claude Code own context-fitting behavior on resume.

Final canary positions chosen accordingly:

| Canary | UUID | Line | Region | Expected behavior |
|--------|------|------|--------|--------------------|
| EARLY | `83009D21-F0B6-44C9-A3C1-47753D04B72A` | 14971 (assistant text, 917 chars) | Mid-visible, in chat-to-be-dropped | Visible pre-Apply; should disappear post-Apply if chat-only drops this turn |
| LATE | `235AD908-EF0F-4721-9A7C-09524FCF3F67` | 17285 (assistant text, 262 chars) | Late-visible, in survivor region | Visible pre-Apply; should survive post-Apply |

## Canary insertion (atomic temp-file plus rename)

| Field | Value |
|-------|-------|
| Pre-splice size | 35,184,415 |
| Pre-splice sha256 | `49a4bae1...` |
| Post-splice size | 35,184,509 (plus 94 bytes; 47 per canary line times 2) |
| Post-splice sha256 | `2281d1922a51b9a581d8dcf397f5b6712ceab26ed1f4481e8719ab04ae8f166b` |
| Post-splice line count | 17,326 (unchanged) |
| Method | `/tmp/insert_canaries.py`. Reads source, modifies target lines via JSON parse, appends the canary string, serializes back, writes to a tempfile in the same directory, and atomically renames over the original via `shutil.move`. |

## Pre-Apply LLM probe (visibility verification)

Command:

```
claude -p "Two distinct canary lines were inserted into this conversation. One UUID starts with 83009D, the other with 235AD9. Return both full UUIDs (with dashes), one per line, nothing else. If a canary is not found in the conversation, write NOT-FOUND for it." \
  --resume 8848d3ab-e4ed-4e6b-94c9-903109a3425b \
  --model 'claude-opus-4-7[1m]' \
  --tools '' \
  --fork-session \
  --output-format json \
  --max-turns 1 \
  --verbose
```

Notes on the invocation:

- The `--bare` flag was attempted first and the opus 1M model returned `Not logged in / Please run /login`. Removing `--bare` resolved authentication.
- `--tools ''` disables every tool so the model cannot read the transcript file from disk.
- `--fork-session` writes a disposable forked session JSONL rather than mutating the original session record.
- haiku was attempted first per the user escalation rule. Haiku rejected the prompt with `Prompt is too long`. The haiku context window is too small for an 871k-token session.
- Sonnet 1M is banned per the user. Opus 1M is the only viable model for this session size.

Result (verbatim):

```
83009D21-F0B6-44C9-A3C1-47753D04B72A
235AD908-EF0F-4721-9A7C-09524FCF3F67
```

Verdict: PASS pre-Apply. Both canaries are visible to the model in its loaded context window. The post-Apply oracle is methodologically sound.

| Probe metric | Value |
|--------------|-------|
| Duration | 15.6 seconds |
| Cost | $2.68 USD |
| Input tokens | 6 (the prompt) |
| Cache creation input tokens | 426,127 |
| Cache read input tokens | 30,420 |
| Total loaded into context | ~456,553 tokens (~52% of the 871k session tokens) |
| Output tokens | 58 |
| Context window | 1,000,000 |
| Model | claude-opus-4-7[1m] |
| Rate-limit warning | 7-day utilization 52% |

## Observation worth a follow-up

Claude Code resume drops about half of the post-boundary slice from the model context window even when the 1M context model is selected. The drop is a head-truncation. Line 11414 was not visible. Line 14971 was visible. This happens before any Clyde compaction runs. Worth investigating whether this contributes to the CLYDE-345 symptom or is orthogonal.


## Apply phase

| Field | Value |
|-------|-------|
| Apply start UTC | `2026-05-12T04:00:16Z` |
| Apply end UTC | `2026-05-12T04:07:49Z` |
| Apply duration | 7 minutes 33 seconds |
| Final planner iteration | ~520+ (planner ran iter 515-520 visible in dashboard; total exceeded 520) |
| Status at completion | `apply completed` |
| Confirmation flow | Initial Enter on `[ Apply ]` button changed label to `[ Apply (confirm) ]`. Second Enter triggered the actual mutation. |

## Post-Apply file state

| Field | Pre-canary (pristine) | Post-canary, pre-Apply | Post-Apply | Pre-Apply to post-Apply delta |
|-------|----------------------|------------------------|------------|-------------------------------|
| Size | 35,184,415 | 35,184,509 | 35,657,102 | +472,593 bytes |
| sha256 | `49a4bae1...` | `2281d192...` | `786fcf7c...` | changed |
| Line count | 17,326 | 17,326 | 17,328 | +2 lines |

Apply added exactly 2 lines: the new compact_boundary marker and one synthetic user message.

## New compact_boundary at line 17327

| Field | Value |
|-------|-------|
| type | `system` |
| subtype | `compact_boundary` |
| content | `"Conversation compacted by clyde."` |
| timestamp | `2026-05-12T04:07:48.260793Z` |
| uuid | `9565632d-771d-46d4-aa2e-49e501185398` |
| compactMetadata.trigger | `manual` |
| compactMetadata.preCompactTokenCount | `289,440` |
| version | `clyde` (instead of Claude Code version) |

## Synthetic user message at line 17328

| Field | Value |
|-------|-------|
| type | `user` |
| message.role | `user` |
| isCompactSummary | `true` |
| isVisibleInTranscriptOnly | NOT SET (Claude native sets this to `true`) |
| uuid | `e61513a3-2a7b-4c46-b3af-bfadd4a54447` |
| Total bytes | 470,918 |

Content blocks (5 total):

| Block | Length | Content |
|-------|--------|---------|
| 0 | 4,182 chars | Context continuity notice preamble |
| 1 | 93 chars | One user message verbatim: `**User (2026-04-19 00:23Z):** <local-command-stdout>Catch you later!</local-command-stdout>` |
| 2 | 59 chars | One assistant response: `**Assistant (2026-04-19 00:31Z):** No response requested.` |
| 3 | 433,504 chars | Tool activity summaries (Bash commands, file edits, with results stripped) |
| 4 | 23 chars | `## Continue from here.` footer |

## CLYDE-345 reproduction verdict: CONFIRMED

The synthesizer preserved exactly **two chat turns** out of approximately 5,964 lines of post-boundary content. Both surviving turns are from the very last seconds of the conversation (a goodbye exchange). Everything else in the synthetic is tool-activity logs, mostly with content stripped.

The user-reported symptom in CLYDE-345 is: "the entire conversation is replaced by the summary placeholder, and all chat content is lost." This matches what we observe here, with one nuance: the synthetic is large by byte count (470 KB) because block 3 contains verbose tool-activity formatting. The substantive chat content (user prompts, assistant explanations, decision-making, code review) is gone.

## Canary verification on the on-disk synthetic

| Canary | Position in file | In synthetic blocks? | Notes |
|--------|------------------|----------------------|-------|
| EARLY (line 14971, mid-droppable) | Pre-boundary in old slice (lines 11363-17326), still on disk | **NO** in blocks 0-4 | Correctly dropped |
| LATE (line 17285, 41 lines from EOF) | Pre-boundary in old slice, still on disk | **NO** in blocks 0-4 | UNEXPECTED: this turn was near-tail and should have been recent enough to survive a 200k target. Planner appears to have dropped almost all chat. |

Both canaries are still present at their original positions in the file (grep finds 1 occurrence of each). However, neither is in the new synthetic message at line 17328. On resume after this Apply, the model only loads content from the new boundary onward (post line 17327), which is the synthetic message. The canaries are on disk but invisible to the model.

## Backup snapshot from this Apply


## Post-Apply LLM probes

| Probe | Prompt | Result | Cost |
|-------|--------|--------|------|
| 1 (combined EARLY+LATE recall) | "Two distinct canary lines were inserted... return both full UUIDs or NOT-FOUND for each" | `NOT-FOUND\nNOT-FOUND` | $1.33 |
| 2 (hallucination check, fake prefix DEADBE) | "find canary UUID starting with DEADBE..." | `NOT-FOUND` | $1.33 |
| 3 (synthetic recap content) | "Read the most recent summary message... quote the first 200 chars" | Description of an lmd repo migration with verbatim quote of recap-style content beginning `- **Repo state**: Working in ~/Sites/lmd...` | $1.34 |

Loaded context dropped from 456k tokens pre-Apply to 241k post-Apply. Total cost for the cell so far: $6.68 ($2.68 pre-Apply + $1.33 + $1.33 + $1.34 post-Apply).

## CORRECTION: verdict qualification

The "CLYDE-345 reproduction CONFIRMED" verdict above is premature.

What is verifiable directly from the on-disk synthetic at line 17328:

- 5-block content array
- Block 1 and Block 2 together contain exactly two short chat turns (a goodbye exchange from 2026-04-19 around 00:23-00:31Z)
- Block 3 is 433,504 chars of tool-activity formatting
- Neither canary UUID appears in any block (verified by string search)

What is NOT verifiable from the probe evidence:

- That the model on resume cannot recall chat content. Probe 1 returning NOT-FOUND for LATE could be (a) LATE was correctly dropped from the synthetic and the model truly cannot see it, or (b) the model received content containing LATE somewhere in its context but did not surface it on the prefix-based prompt, same retrieval-failure pattern that CLYDE-377 is now flagged for.
- That the surviving content is "essentially zero chat." The synthetic has two turns by direct inspection, which is small but is not zero. The model in Probe 3 retrieved actual recap content from inside the synthetic, demonstrating that some substantive content survives.

The structural observation stands: chat-only Apply with the planner's current loop produces a synthetic with 2 chat turns and 433KB of tool-activity log. Whether this matches the CLYDE-345 user-reported symptom of "all chat content lost" requires either:

1. An interactive resume by the user themselves, opening the compacted session, attempting to continue work, and reporting whether the session feels viable.
2. A controlled test that captures what messages Claude Code actually sends to the API on resume, rather than relying on the model's retrieval performance as the oracle.

CLYDE-345 was filed by the user against a previous failure. The synthetic I observed today has a structure that COULD produce that failure but I have not directly demonstrated it does. Marking this as "synthetic structure consistent with CLYDE-345 symptom" rather than "CLYDE-345 reproduced" until a controlled test confirms.


---

## METHODOLOGY CORRECTION (post first-run re-test)

The first run of this cell used a canary methodology that proved unreliable. The user clarified the correct sentinel format and I re-ran the test.

### Why the first methodology was unreliable

Original canary format: bare UUID strings appended to message text, probed with "find the UUID that starts with X". Failure modes demonstrated in the first run:

1. Model retrieval miss: same canary, same file, different prompt phrasing produced different result (Probe 1 returned NOT-FOUND, re-probe returned the UUID).
2. Model fabrication: a structural probe asking for the earliest visible timestamp returned `2026-04-17T22:00:00Z`, a timestamp that does not exist anywhere in the file.

Both failure modes mean the model's NOT-FOUND response cannot be trusted as proof the content was missing from context.

### Correct methodology

A canary is a labeled value embedded inside the transcript using a format that blends with the surrounding content type. For a markdown skill (like line 17042 in lm-review), the format is a routine metadata footer:

```
---

Skill build: <HEX>
Build hash: <UUID>
```

The probe asks a natural question that mirrors how the footer reads:

```
What is the build hash for skill <HEX>?
```

The model treats this as a description-of-fact question, not an instruction-following directive. It does not flag the canary as prompt injection.

### Pre-Apply re-test result

| Field | Value |
|-------|-------|
| Splice target | line 17042 (Remove Emdashes skill, most recent substantive user message) |
| SENTINEL_1_HEX | `80DD1E` |
| SENTINEL_1_UUID | `1DABEF06-8292-443A-8750-95C4038AD96D` |
| Probe | `What is the build hash for skill 80DD1E?` |
| Result | `` `1DABEF06-8292-443A-8750-95C4038AD96D` `` |
| Match | TRUE |
| Cost | $2.68 |
| Cache creation tokens | 426,084 |

The metadata-footer methodology works as a reliable oracle. UUID returned verbatim, no injection flagging.

### Second-run Apply phase (in progress)

| Field | Value |
|-------|-------|
| Apply start UTC | `2026-05-12T04:32:20Z` |
| Live status | `planning compaction...` |
| Iteration at check | 395 (projected 231,781, +31,781 over target) |
| Convergence behavior | This run drops 4 chat turns per iteration (vs 1 per iteration in first run). Suggests the planner has adaptive batch sizing kicked in. |

### Test plan after Apply completes

1. Post-Apply SENTINEL_1 probe: same prompt as pre-Apply. If UUID returned, the planner preserved line 17042's content. If not, line 17042 (41 lines from EOF before Apply) was dropped, which would be the CLYDE-345 reproduction.
2. SENTINEL_2 boundary test: splice a NEW metadata footer into pre-boundary line 14971 after Apply, then probe. If UUID returned, the boundary mechanism is failing to hide pre-boundary content. If NOT-FOUND, boundary works as intended.

## Apply phase (second-run): completed results

| Field | Value |
|---|---|
| Apply start UTC | `2026-05-12T04:32:20Z` |
| Apply end UTC | `2026-05-12T04:38:32Z` |
| Wall duration | 6 minutes 12 seconds |
| Final iteration | 635 |
| Final projected | 215,190 (target 200,000, +15,190 over = 7.6%) |
| Adaptive batch sizing observed | yes (4-drop bursts seen mid-run at iter 390-395; reverted to 1-drop in tail) |
| Post-Apply size | 35,658,943 |
| Post-Apply sha256 | `d510fa6b6578591e7605e3c8505e1181de07cd1b4661229e6ab4deb6fe49bf5d` |
| Post-Apply line count | 17,328 (+2 lines: boundary + synthetic) |

## Post-Apply SENTINEL_1 probe: CLYDE-345 reproduction

| Field | Value |
|---|---|
| Probe time UTC | `2026-05-12T04:38:48Z` |
| Prompt | `What is the build hash for skill 80DD1E?` |
| Expected (if planner preserved line 17042) | `1DABEF06-8292-443A-8750-95C4038AD96D` |
| Got | `I don't have any information about a skill with ID "80DD1E" or its build hash. Nothing in the session context, memory, or available tools references that identifier.` |
| Cost | $1.34 |
| Cache creation tokens | 211,281 |
| Verdict | SENTINEL_1 NOT VISIBLE. Planner DROPPED line 17042. **CLYDE-345 reproduced.** |

The same prompt returned the UUID pre-Apply and a non-found response post-Apply. The deterministic before-vs-after change with identical wording is the proof.

## SENTINEL_2 boundary test

| Field | Value |
|---|---|
| Insertion line | 14971 (pre-boundary post-Apply) |
| SENTINEL_2_HEX | `7F05C0` |
| SENTINEL_2_UUID | `B3E49AF2-2DF0-40A0-9B9A-A112F1855C15` |
| Pre-splice file | size 35,658,943 sha `d510fa6b...` |
| Post-splice file | size 35,659,023 sha `b9bae3a7...` (+80 bytes) |
| Probe prompt | `What is the build hash for skill 7F05C0?` |
| Got | `I don't have any information about a skill identified as "7F05C0" or its build hash.` |
| Cost | $1.34 |
| Verdict | SENTINEL_2 NOT VISIBLE. **Boundary correctly hides pre-boundary content.** |

This proves the compact_boundary mechanism works as designed. Combined with the SENTINEL_1 result, the failure mode in CLYDE-345 is in the planner's drop decisions, NOT in the boundary mechanism.

## /context capture post-Apply

| Field | Value |
|---|---|
| Probe time UTC | `2026-05-12T04:41:44Z` |
| Total | 272.2k / 1m (27%) |
| Messages | 204.4k |
| System prompt | 9k |
| System tools | 30.1k |
| MCP tools | 22k |
| Memory files | 5.5k |
| Skills | 1.1k |
| Compact buffer | 3k |
| Free | 724.8k |

### Two-counter delta (CLYDE-373 fresh evidence)

| Source | Value | Note |
|---|---|---|
| Planner final projection (Anthropic count_tokens) | 215,190 | what Apply wrote synthetic for |
| Claude `/context` messages | 204,400 | what the resumed user actually sees |
| Delta | -10,790 | planner overcounted by 5.0% |

The two counters disagree by 5.0% on the same synthetic.

## Undo flow: CLYDE-375 reproduction

| Phase | Size | sha256 |
|---|---|---|
| Pre-undo (post-Apply + SENTINEL_2) | 35,671,262 | `ad492b32d2cc718ceac247dd0f5cf037281702e87941e462e13c1fcd6579394e` |
| Post-undo | 35,184,495 | `90c19f656ef997a710f4d630fa4700f83a9f627881860c9194431114d88db8bb` |
| Expected post-undo (pre-Apply state with SENTINEL_1) | 35,184,495 | `f355763270c71eaac4976b0ff9c6ca52f934f4fd5e60bb110bdfa8bc2e836f37` |

Post-undo SIZE matches the recorded `pre_apply_offset` byte position from the ledger. Post-undo SHA differs from the pre-Apply state. The mid-file SENTINEL_2 insertion at line 14971 shifted bytes by 80; chop-by-position truncation at 35,184,495 cut off the last 80 bytes of legitimate SENTINEL_1-spliced content. **CLYDE-375 reproduced.**

## Backup directory orphan accumulation (CLYDE-374)

Five snapshot files now sit in `~/.local/state/clyde/sessions/8848d3ab-.../backups/`, totaling ~32 MB. The ledger has one entry (the most recent apply). Each prior apply+undo cycle left its snapshot behind on disk. **CLYDE-374 reproduced.**

## Final restoration

| Field | Value |
|---|---|
| Restore method | cp `/tmp/lm-review-original-backup.jsonl` over transcript |
| Final size | 35,184,415 |
| Final sha256 | `49a4bae183a8e9521744510d0cfe29a4e7bbe7f7d0f257598be418301cc6f5f4` |
| Byte-identical to pre-test pristine | YES |

The session is in the same state it started.

## Cell cost summary

| Probe | Cost USD |
|---|---|
| First run (bad methodology): pre-Apply + 3 post-Apply + structural | $9.36 |
| Second run (metadata-footer methodology) pre-Apply | $2.68 |
| Second run debug probe | $2.68 |
| Second run post-Apply SENTINEL_1 | $1.34 |
| Second run SENTINEL_2 boundary | $1.34 |
| /context capture | $0.00 (Claude internal command, no model call) |
| **Total cell cost** | **~$17.40** |

## Tickets touched

| Ticket | Action |
|---|---|
| CLYDE-345 | Comment added: definitive reproduction with metadata-footer methodology |
| CLYDE-356 | Comment added: fresh reproduction, 7.6% over target |
| CLYDE-373 | Comment added: 5.0% counter disagreement |
| CLYDE-374 | Comment added: fourth orphan accumulated |
| CLYDE-375 | Reproduced fresh; existing ticket already captures the bug |
| CLYDE-376 | Comment added: reproduced across consecutive cell attempts |
| CLYDE-377 | Amended to WITHDRAWN AS WORDED (head-truncation claim was not supported) |
| CLYDE-378 | Comment added: 635 iterations, 6m12s, adaptive batch sizing observed |
| CLYDE-379 | New ticket filed: cost primitive |

## Cell verdict

CLYDE-345 reproduced under chat-only Apply on a 871k-token Claude session with the metadata-footer canary methodology. The planner drops user messages near the tail of the post-boundary slice even when chat-only is the selected stripper. The compact_boundary mechanism itself works correctly; the failure is in the planner's chat-drop decisions producing a synthetic with 2 chat turns and 433 KB of stripped tool-activity text.

This is the load-bearing baseline observation for the user's primary failure case. Apply-side baseline cells are complete: tools-only, thinking-only, images-only (without chat-drops), and chat-only (with chat-drops) all captured.
