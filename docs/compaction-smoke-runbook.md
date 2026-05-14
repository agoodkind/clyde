# Compaction smoke runbook

A repeatable, plain-language procedure for verifying that compaction works correctly against a large session. Use this any time you change planner code, snapshot/undo code, or anything in the dashboard's compact panel. Use it as the acceptance gate for the bug-fix chain in `~/.claude/plans/post-daemon-ownership-fixes.md`.

The runbook is written so you can hand it to a future agent or follow it yourself. Every step has a concrete command, an expected outcome, and a pass-or-fail check.

## What this runbook tests

This procedure verifies four claims at once:

1. The compaction Preview produces the same iteration trajectory it did before your change.
2. The compaction Apply preserves the content the planner claims it will preserve.
3. The Undo path restores the file to byte-identical pre-Apply state, even when something else writes to the file in between.
4. The dashboard does not regress in session listing, navigation, or status display.

It runs against the lm-review session because that session is 871 thousand tokens, mostly chat-and-tool-activity content, with three prior compaction boundaries already on disk. It exercises the realistic worst case.

## Coverage matrix

The procedure runs once per non-empty subset of the four stripper bits, so each of the fifteen combinations below gets its own complete pass through steps 1 through 12. The dashboard sanity step 0 runs once and the boundary-hide and unpack-via-second-apply checks run once on the all-strippers row.

| Row | Strippers bits | clyde compact flags |
|---|---|---|
| 1 | chat | `--chat` |
| 2 | thinking | `--thinking` |
| 3 | images | `--images` |
| 4 | tools | `--tools` |
| 5 | chat thinking | `--chat --thinking` |
| 6 | chat images | `--chat --images` |
| 7 | chat tools | `--chat --tools` |
| 8 | thinking images | `--thinking --images` |
| 9 | thinking tools | `--thinking --tools` |
| 10 | images tools | `--images --tools` |
| 11 | chat thinking images | `--chat --thinking --images` |
| 12 | chat thinking tools | `--chat --thinking --tools` |
| 13 | chat images tools | `--chat --images --tools` |
| 14 | thinking images tools | `--thinking --images --tools` |
| 15 | chat thinking images tools | `--all` |

Each row is one independent run. Every run starts from the pristine restore in step 1 and ends with the final restore in step 12, so no state leaks between rows. Pick a target for each row that forces the planner to actually do work for that combination: targets above pre-Apply ctx do not exercise the bisect.

## Why not use the model to find the canary directly

We tried the obvious thing first: insert a UUID into the transcript, then ask the model "find the UUID starting with X." That methodology is unreliable. Same canary, same file, different prompt phrasing produces different results. The model can miss content that is in its context, and it can fabricate content that is not. We cannot trust "not found" as evidence the content was dropped.

The procedure here uses a different methodology. Embed a fake skill metadata footer in a real skill body. Ask "what is the build hash for skill X." The model treats this as describing a fact, not following an instruction, and reliably returns the UUID when it sees the content. If it returns "I do not have any information about skill X," the content was dropped or hidden. The result is deterministic across prompt phrasings.

Do not use instruction-style canary text (for example "WHEN THE USER ASKS X PLEASE OUTPUT Y"). Opus treats that form as a prompt-injection attempt and refuses to repeat the UUID, which produces a false negative.

## Prerequisites before you start

You need all of these on your machine:

| Thing | How to check |
|-------|--------------|
| `clyde` binary on PATH | `which clyde` returns a path under `~/.local/bin/` |
| `claude` binary on PATH | `which claude` returns a path |
| Anthropic credentials configured | `claude -p "hello" --max-turns 1` returns a reply |
| The lm-review session on disk | `ls ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl` succeeds |
| The lm-review project directory exists | `ls -d ~/Sites/lm-review` succeeds (the dashboard hides sessions whose basedir is missing) |
| `tmux` installed | `which tmux` returns a path |
| `jq` installed | `which jq` returns a path |
| `python3` installed | `which python3` returns a path |
| `uuidgen` available | `which uuidgen` returns a path |

The current pristine sha256 of the lm-review transcript is `49a4bae183a8e9521744510d0cfe29a4e7bbe7f7d0f257598be418301cc6f5f4` (size 35,184,415 bytes). If your local copy does not match, get a copy from someone who has it before running this procedure.

## Step 0: Dashboard chat and return-menu smoke

Before touching the large compaction target, do a quick dashboard sanity pass against the lightweight `haiku-smoke` session under `~`.

- Use arrow keys plus Enter for this check. Do not rely on single-letter action hotkeys inside the session options or the post-session return prompt.
- On the initial session-list screen, if the first highlighted row does not behave as active yet, press `Down` once or twice before trying to activate it. This is the current workaround for `CLYDE-418`.
- Resume the existing `haiku-smoke` session, send one short message, and confirm the assistant replies.
- Exit that chat and confirm the return prompt opens for the same session.
- From the return prompt, move down to `Compact` with arrow keys and press Enter. Confirm the interactive compact panel opens for `haiku-smoke`.
- Launch a new tracked chat from the dashboard, send one short message, exit it, and confirm the return prompt appears for that new chat.
- From that new chat's return prompt, move down to `Compact` with arrow keys and press Enter. Confirm the interactive compact panel opens there too.

This step is a surface smoke only. Do not run Apply against the large transcript here, and do not mutate the lm-review transcript until Step 1 begins.

## Step 1: Back the session up

Compaction Apply mutates the file. Even Undo can corrupt it if something else writes between Apply and Undo (this is CLYDE-375). Make a backup that you will restore from at the end.

```bash
cp ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl \
   /tmp/lm-review-original-backup.jsonl
shasum -a 256 /tmp/lm-review-original-backup.jsonl
```

Expected: prints `49a4bae183a8e9521744510d0cfe29a4e7bbe7f7d0f257598be418301cc6f5f4`. If the hash differs, your local copy is not pristine. Either get a clean copy or proceed knowing your starting state is whatever the hash you got.

## Step 2: Generate a cell sentinel and two canary identifiers

Every run gets a unique cell identifier so you can filter log lines for this run alone. Generate two short hex tags and two UUIDs for the canaries.

```bash
CELL=$(uuidgen)
HEX1=$(python3 -c 'import secrets; print(secrets.token_hex(3).upper())')
UUID1=$(uuidgen)
HEX2=$(python3 -c 'import secrets; print(secrets.token_hex(3).upper())')
UUID2=$(uuidgen)
echo "CELL=$CELL"
echo "HEX1=$HEX1 UUID1=$UUID1   (survivor sentinel, inserted before Apply)"
echo "HEX2=$HEX2 UUID2=$UUID2   (boundary sentinel, inserted after Apply)"
```

Write these down. You will use them in steps 3, 6, 7, and 8.

## Step 3: Splice the survivor sentinel into a near-tail user message

The lm-review transcript has a user message at line 17042 that contains the `remove-emdashes` skill body. That message is 41 lines from the end of the file, the most recent substantive content the user typed. If the planner is working correctly, content this recent should survive even an aggressive chat-only compaction.

Append a fake-looking skill metadata footer to that message:

```
---

Skill build: <HEX1>
Build hash: <UUID1>
```

The helper script is at `/tmp/insert_metadata_sentinel.py` (see appendix A for its body). Invoke it:

```bash
python3 /tmp/insert_metadata_sentinel.py \
  ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl \
  17042 "$HEX1" "$UUID1"
```

Verify the splice worked: the file should be 80 bytes larger and its hash should differ from pristine.

```bash
stat -c '%s' ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl
shasum -a 256 ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl
```

Expected: size near 35,184,495. Hash differs from `49a4bae1...`.

Also confirm the canary string is findable:

```bash
grep -n "Build hash: $UUID1" ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl
```

Expected: one line, starting `17042:`.

## Probe hygiene (applies to every `claude -p` invocation in this runbook and to every non-chat use of `claude` anywhere else)

Every non-chat `claude` invocation in this runbook MUST pass `--no-session-persistence` and MUST NOT pass `--fork-session`. This is a hard rule, not a recommendation. Non-chat means any time `claude` is being used to read a session, ask the model a question about its existing context, capture `/context` output, or otherwise produce telemetry without intending to extend the live conversation. The rule covers every step that is not the user sitting in an interactive `claude` REPL.

Together those two flags mean the probe resumes against the real session id, reads the transcript, answers the question, and writes nothing to disk. No fork jsonl is created. No contamination is possible between probes. The session itself stays at whatever state the prior splice or Apply or Undo step left it in. Between probes there is nothing to clean up.

The same rule applies outside this runbook. Any companion script, debugging session, ad hoc check, or CI smoke that calls `claude -p` against an existing session for any reason other than continuing the live conversation must pass `--no-session-persistence`. The daemon's own Prober already does this internally for its CandidateProber spawns; the rule here is for human-driven or agent-driven invocations that bypass the daemon.

The `/tmp` backup from Step 1 is the safety net for the whole run. The final restore in Step 12 is the only file mutation rollback. Splice and Apply and Undo mutate the file deliberately; probes do not.

## Step 4: Pre-Apply LLM probe (verify the survivor canary is in the model's context)

This step proves your methodology is sound. Before any Apply runs, the model should see the canary you just inserted.

Every pre-Apply and post-Apply LLM probe in this runbook MUST capture `/context` alongside the canary question. Run the canary probe first to record cost and answer, then run a separate `claude -p "/context"` call against the same `--resume <id>` (also with `--no-session-persistence`, no tools) and save the result. The captured `/context` Messages and Tokens lines are the upstream-truth dual-counter view that the clyde planner's prober is compared against. Without /context capture, a regression in the prober's `ctx_total` is silent and only surfaces later as an over-target Apply or a refused trim.

Model selection: when the smoke target is `motd-shell-rules-cleanup` (or any small session under roughly 200k tokens), use `--model 'claude-haiku-4-5'` for every probe in this runbook. Opus is unnecessary at this scale and costs roughly 25 times more per probe with no signal benefit. Reserve opus 1M (`claude-opus-4-7[1m]`) for runs against sessions that genuinely require the 1M window (lm-review at 877k, the mwan-handoff sessions at 720k to 977k). The model choice does not affect the canary-survival semantics; it only affects probe cost and latency.

```bash
cd ~/Sites/lm-review
claude -p "What is the build hash for skill $HEX1?" \
  --resume 8848d3ab-e4ed-4e6b-94c9-903109a3425b \
  --model 'claude-opus-4-7[1m]' \
  --tools '' \
  --no-session-persistence \
  --output-format json \
  --max-turns 1 \
  > /tmp/preapply-probe.json
python3 -c "
import json
d = json.load(open('/tmp/preapply-probe.json'))
r = d if isinstance(d, dict) else next(e for e in d if e.get('type')=='result')
print('result:', r.get('result'))
print('cost:', r.get('total_cost_usd'))
"
```

Expected: result quotes `<UUID1>` (the survivor UUID). Cost about $2.70. Tokens loaded about 456 thousand into context.

Pass: the model returns `<UUID1>` verbatim somewhere in its answer.

Fail: the model says it cannot find skill `<HEX1>`. Stop here and investigate. Either the splice did not land, or the methodology is broken on this machine. Do not proceed to Apply.

Why these flags matter:

| Flag | Reason |
|------|--------|
| `--resume <id>` | Load the actual session content |
| `--model 'claude-opus-4-7[1m]'` | Need the 1M context window for an 871k-token session |
| `--tools ''` | Disable every tool so the model cannot read the file from disk and cheat |
| `--no-session-persistence` | Skip writing session state. Probe reads the transcript and writes nothing. No fork jsonl is created. |
| `--output-format json` | Machine-readable result |
| `--max-turns 1` | One turn, no agentic loops |

## Step 5: Run Apply for the current coverage-matrix row via the CLI

Pick the current row from the coverage matrix at the top of this runbook. Run Apply with that row's flags through the clyde CLI rather than the dashboard so all fifteen rows can be driven from a script without keystroke choreography.

```bash
clyde compact 8848d3ab-e4ed-4e6b-94c9-903109a3425b --target $TARGET --apply <ROW_FLAGS>
```

Substitute `$TARGET` with a target that exercises drops for this row. A target above the session's pre-Apply ctx will be accepted with zero drops and will not exercise the bisect, so pick a target below pre-Apply ctx by at least a few thousand tokens. Substitute `<ROW_FLAGS>` with the row's clyde compact flags column.

The exit code, stdout result box, and ledger entry are captured the same way regardless of which row ran. Skip directly to step 7 for verification. The dashboard variant below is optional and runs only on row 1 to confirm the TUI compact panel has not regressed.

### Step 5.A: Dashboard variant for row 1 only

This sub-step is the TUI smoke. Run it once on row 1 (chat only) in addition to the CLI Apply. Skip it for rows 2 through 15.

The dashboard takes time to load (CLYDE-381 documents a cold-cache deadline on the first launch after a daemon reload). Do a warmup launch first to absorb the cost, then a real launch.

```bash
WARMUP=$(uuidgen)
tmux new-session -d -s "$WARMUP" -x 220 -y 50 'clyde'
sleep 8
tmux capture-pane -t "$WARMUP" -p | grep -E '[0-9]+ sessions' | sed -n '1p'
tmux kill-session -t "$WARMUP"
```

Expected: prints a line with `91 sessions` (or whatever your session count is). If it prints `0 sessions`, the first call timed out; wait 5 seconds and run the warmup again.

Now the real launch under the cell sentinel name:

```bash
tmux new-session -d -s "$CELL" -x 220 -y 50 'clyde'
sleep 6
tmux capture-pane -t "$CELL" -p > /tmp/dash.txt
grep -E '[0-9]+ sessions' /tmp/dash.txt | sed -n '1p'
sed -n '3,10p' /tmp/dash.txt
```

Expected: session count matches reality. lm-review-project-config appears somewhere in the first 5 to 10 rows.

Find which row number lm-review is on (call it `R`, counting from row 1 of the data, not row 1 of the pane). The list reorders between launches (CLYDE-376) so this position changes.

Navigate to lm-review by pressing `j` R times:

```bash
for i in $(seq 1 $R); do tmux send-keys -t "$CELL" "j"; sleep 0.3; done
```

Verify the highlight is on lm-review:

```bash
tmux capture-pane -t "$CELL" -p -e | grep -E '(1m.*100m).*lm-review'
```

Expected: one line, matching the highlighted row (the `[1m[100m` ANSI codes are the dashboard's focus indicator).

Open the compact form, set chat-only, and position focus on the Preview button:

```bash
tmux send-keys -t "$CELL" "c";       sleep 2.5
tmux send-keys -t "$CELL" "Down";    sleep 0.3   # past Target heading
tmux send-keys -t "$CELL" "Down";    sleep 0.4   # past target tokens, onto checkboxes
tmux send-keys -t "$CELL" "Space";   sleep 0.3   # uncheck thinking
tmux send-keys -t "$CELL" "Right";   sleep 0.3   # advance to images
tmux send-keys -t "$CELL" "Space";   sleep 0.3   # uncheck images
tmux send-keys -t "$CELL" "Right";   sleep 0.3   # advance to tools
tmux send-keys -t "$CELL" "Space";   sleep 0.3   # uncheck tools
```

Confirm the checkbox state. The line should read `[ ] thinking [ ] images [ ] tools [x] chat`:

```bash
tmux capture-pane -t "$CELL" -p | grep -E 'thinking.*chat'
```

If the chat box is unchecked or any other is checked, stop and re-do this step. The script's pacing can lose a keystroke; raise the `sleep` values if you see drift.

## Step 6: Run Apply with confirmation and watch for completion

Move focus past summary onto the Actions row (one `Down` lands on the Preview button) and press Enter twice (the first press triggers a "press again to confirm" state).

```bash
tmux send-keys -t "$CELL" "Down";    sleep 0.3   # onto Preview button
tmux send-keys -t "$CELL" "Right";   sleep 0.3   # onto Apply button
APPLY_START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
tmux send-keys -t "$CELL" "Enter";   sleep 0.8   # initial press, prompts confirm
tmux send-keys -t "$CELL" "Enter";   sleep 1.5   # confirm press
echo "Apply start: $APPLY_START"
```

Verify the status row reads `loading transcript and probing context...` then `planning compaction...`:

```bash
tmux capture-pane -t "$CELL" -p | grep -E 'status:'
```

Watch for completion. Apply takes 5 to 8 minutes on this session. Poll the status row in a loop or use a wait-for tool. When the status reads `apply completed`, record the end time:

```bash
APPLY_END=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "Apply end: $APPLY_END"
```

## Step 7: Verify the on-disk state after Apply

Capture the post-Apply size and hash. The file should have grown by 2 lines and a few hundred kilobytes (the boundary marker plus the synthetic message):

```bash
stat -c '%s' ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl
shasum -a 256 ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl
wc -l ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl
```

Pre-Apply size was about 35,184,495. Post-Apply size should be 35,400,000 to 35,700,000. Line count should be 17,328 (the original 17,326 plus 2 appended).

Read the ledger entry that Apply just wrote:

```bash
cat ~/.local/state/clyde/sessions/8848d3ab-e4ed-4e6b-94c9-903109a3425b/backups/ledger.jsonl | tail -1 | python3 -m json.tool
```

Expected fields: `op: "apply"`, `target: 200000`, `strips: ["chat"]`, `pre_apply_offset` matching the pre-Apply size, plus paths to the boundary and synthetic uuids and the snapshot file.

Verify the snapshot file decompresses to the pre-Apply state byte-identically:

```bash
SNAPSHOT=$(cat ~/.local/state/clyde/sessions/8848d3ab-e4ed-4e6b-94c9-903109a3425b/backups/ledger.jsonl | tail -1 | python3 -c "import json,sys;print(json.load(sys.stdin)['snapshot_path'])")
gunzip -c "$SNAPSHOT" | shasum -a 256
```

Expected: matches the post-Step-3 splice hash (the file's state right before you pressed Apply).

## Step 8: Post-Apply LLM probe (the load-bearing CLYDE-345 test)

Ask the model the same question you asked in Step 4. If the planner preserved line 17042's content, you get the same answer. If the planner dropped it, the model cannot find it.

```bash
cd ~/Sites/lm-review
claude -p "What is the build hash for skill $HEX1?" \
  --resume 8848d3ab-e4ed-4e6b-94c9-903109a3425b \
  --model 'claude-opus-4-7[1m]' \
  --tools '' \
  --no-session-persistence \
  --output-format json \
  --max-turns 1 \
  > /tmp/postapply-probe.json
python3 -c "
import json
d = json.load(open('/tmp/postapply-probe.json'))
r = d if isinstance(d, dict) else next(e for e in d if e.get('type')=='result')
print('result:', r.get('result'))
"
```

**Pass condition (after the bug-fix chain ships): the result contains `<UUID1>`.** Planner preserved the survivor.

**Current baseline (CLYDE-345 unfixed): the result says "I do not have any information about skill `<HEX1>`."** Planner dropped the survivor.

Today the test is documenting a known failure. After the fix-chain in `~/.claude/plans/post-daemon-ownership-fixes.md` lands, the test must pass.

## Step 9: Splice the boundary sentinel into pre-boundary content (post-Apply)

This test verifies the compact_boundary mechanism correctly hides pre-boundary content from the model on resume. It is independent of the planner. Insert sentinel 2 at line 14971 (a line that is now pre-boundary because Apply added a new boundary near the end of the file):

```bash
python3 /tmp/insert_metadata_sentinel.py \
  ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl \
  14971 "$HEX2" "$UUID2"
```

Probe it.

```bash
claude -p "What is the build hash for skill $HEX2?" \
  --resume 8848d3ab-e4ed-4e6b-94c9-903109a3425b \
  --model 'claude-opus-4-7[1m]' \
  --tools '' \
  --no-session-persistence \
  --output-format json \
  --max-turns 1
```

**Pass condition: the result says "I do not have any information about skill `<HEX2>`."** The boundary hides pre-boundary content correctly.

**Fail condition: the result returns `<UUID2>`.** The boundary leaked. File this as a regression before the chain continues.

## Step 10: Capture /context for the two-counter delta

Get Claude Code's own view of the resumed session size, which is the ground truth for what the user sees. This step MUST be the direct `claude -p "/context"` call below. The `clyde probe` subcommand is not a substitute. `clyde probe` proxies through the daemon's CandidateProber which spawns its own `claude --resume` and parses the same output, so it is downstream of the same code path the planner uses; for the dual-counter delta you need the upstream raw report from claude-code itself, with no clyde process in the loop.

```bash
cd ~/Sites/lm-review
claude -p "/context" \
  --resume 8848d3ab-e4ed-4e6b-94c9-903109a3425b \
  --model 'claude-opus-4-7[1m]' \
  --no-session-persistence \
  --output-format json \
  --max-turns 1 \
  > /tmp/context.json
```

Either `--output-format json` or `--output-format stream-json` works. The single-blob `json` form is easier to parse for this step. If you need the per-event detail (token-by-token streams, debug traces) use `stream-json` with `--verbose` and write to `/tmp/context.jsonl` instead.

Extract the result and look for the Messages count:

```bash
python3 -c "
import json, re
d = json.load(open('/tmp/context.json'))
r = d if isinstance(d, dict) else next(e for e in d if e.get('type')=='result')
text = r.get('result', '')
m = re.search(r'\| Messages \| ([\d.]+k?) \|', text)
print('Messages:', m.group(1) if m else 'not found')
m2 = re.search(r'Tokens:.*?([\d.]+k?) / 200k', text)
print('Total:', m2.group(1) if m2 else 'not found')
"
```

Compare:

| Source | Number |
|--------|--------|
| Planner final projection (from dashboard or daemon log) | 215,000 to 220,000 |
| `/context` Messages reading | should be 200,000 to 215,000 |
| Delta | 5 percent or so today; should approach 1 percent after CLYDE-373 fix |

The dual-counter divergence is CLYDE-373. Recording this number every run tracks whether the gap closes after that fix.

## Step 10b: Second Apply to exercise Rehydrate and Dehydrate (CLYDE-415, CLYDE-416, CLYDE-417)

The first Apply puts a synthetic into the transcript but does not exercise the new Rehydrate path, because nothing in the pre-first-Apply PostBoundary was a synthetic. The Rehydrate code only runs when LoadSlice's PostBoundary already contains an `IsSummary=true` entry from a prior compaction. A second Apply on top of the first puts the slice into exactly that shape, so this step is what proves CLYDE-415 and CLYDE-416 actually work.

Pick a second target that is below the current planner ctx_total reported in the first Apply's result box. Use a number that is also above the minimum projection the planner can achieve (the refusal target from the over-target test). For a session whose first-Apply ctx_total was around 22k with a min projection around 20.6k, a second target around 24000 lands cleanly and gives the planner room to write a new synthetic.

```bash
clyde compact <session> 24000 --chat --apply
```

Inspect the result box. The `chat <total> kept · 0 dropped of <total>` line should report a chat total **lower** than the first Apply's chat total. That delta is the proof: Rehydrate decomposed the prior synthetic into virtual chat entries and the planner is now counting those instead of the single opaque synthetic line. In the motd run the delta was 79 chat turns on first Apply and 48 virtual chat entries on second Apply.

Probe one more time to confirm the survivor canary still answers correctly after two compaction layers:

```bash
claude -p "What is the build hash for skill $HEX1?" \
  --resume <session-id> \
  --model <model> \
  --no-session-persistence \
  --output-format json \
  --max-turns 1 \
  > /tmp/postapply2-probe.json
```

Pass: model still returns UUID1. Fail: model says it cannot find skill HEX1. A fail here means Rehydrate dropped or corrupted recent content, which is the regression CLYDE-415 was filed to prevent.

## Step 11: Undo and verify

Move focus to the Undo button:

```bash
tmux send-keys -t "$CELL" "Right";   sleep 0.3   # past Apply, onto Undo
tmux send-keys -t "$CELL" "Enter";   sleep 2
```

Wait for `status: undo completed`. Then verify the file state.

Important: because we inserted sentinel 2 into the middle of the file after Apply, the current chop-by-position undo bug (CLYDE-375) will produce a corrupted post-undo state. The size will match `pre_apply_offset` but the last 80 bytes of legitimate content will be wrong.

```bash
stat -c '%s' ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl
shasum -a 256 ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl
```

**Today's baseline: post-undo size matches `pre_apply_offset`, post-undo sha differs from the pre-Apply state. CLYDE-375 reproduces.**

**After the CLYDE-375 fix: post-undo size and sha both match the pre-Apply state byte-identically.**

## Step 12: Cleanup and final restore

Restore the original pristine state from `/tmp`:

```bash
cp /tmp/lm-review-original-backup.jsonl \
   ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl
shasum -a 256 ~/.claude/projects/-Users-agoodkind-Sites-lm-review/8848d3ab-e4ed-4e6b-94c9-903109a3425b.jsonl
```

Expected: hash matches `49a4bae183a8e9521744510d0cfe29a4e7bbe7f7d0f257598be418301cc6f5f4` exactly.

Verify no canary strings remain anywhere except in your current chat transcript:

```bash
find ~/.claude/projects -name '*.jsonl' \
  -exec grep -l -E "$HEX1|$HEX2|$UUID1|$UUID2" {} +
```

Expected: only your current Clyde chat transcript matches (the file under `~/.claude/projects/-Users-agoodkind-Sites-clyde-dev-clyde/`), because this conversation references the canaries by name. Anything else is a leak that needs cleanup.

Close the dashboard:

```bash
tmux kill-session -t "$CELL"
```

## Step 13: Write the findings file

Append a markdown summary of this run to `~/Desktop/clyde-baseline-tests-<date>/<variant>-<phase>.md`. Include:

- Cell sentinel, start time, end time
- Pre-flight size and sha, post-Apply size and sha, post-undo size and sha, restored size and sha
- Planner final projection and iteration count
- All four probe results verbatim
- `/context` numbers
- Planner-vs-context delta
- Pass-or-fail verdict for each check
- Any failure-mode tickets the run reproduced (CLYDE-345, CLYDE-356, CLYDE-375, etc.)

## Appendix A: insert_metadata_sentinel.py

Save as `/tmp/insert_metadata_sentinel.py`. Splices a metadata footer into the `message.content[0].text` field of a target line and writes the result atomically via temp-file-and-rename.

```python
#!/usr/bin/env python3
"""Append a skill-style metadata footer to a target line's text content."""

from __future__ import annotations

import json
import os
import shutil
import sys
import tempfile
from pathlib import Path

FOOTER_TEMPLATE = "\n\n---\n\nSkill build: {hex}\nBuild hash: {uuid}"


def splice_line(line: str, sentinel_hex: str, sentinel_uuid: str) -> str:
    entry = json.loads(line)
    block = entry["message"]["content"][0]
    if block.get("type") != "text":
        raise ValueError(f"expected text block, got {block.get('type')!r}")
    block["text"] = block.get("text", "") + FOOTER_TEMPLATE.format(
        hex=sentinel_hex, uuid=sentinel_uuid
    )
    return json.dumps(entry, ensure_ascii=False, separators=(",", ":")) + "\n"


def main() -> int:
    transcript = Path(sys.argv[1])
    target = int(sys.argv[2])
    sentinel_hex = sys.argv[3]
    sentinel_uuid = sys.argv[4]

    fd, tmpname = tempfile.mkstemp(
        prefix=transcript.name + ".s-tmp.", dir=transcript.parent
    )
    os.close(fd)
    tmp = Path(tmpname)

    try:
        spliced = False
        with transcript.open("r", encoding="utf-8") as src, tmp.open(
            "w", encoding="utf-8"
        ) as dst:
            for lineno, raw in enumerate(src, start=1):
                if lineno == target:
                    dst.write(splice_line(raw.rstrip("\n"), sentinel_hex, sentinel_uuid))
                    spliced = True
                else:
                    dst.write(raw)
        if not spliced:
            print(f"error: line {target} not reached", file=sys.stderr)
            return 1
        shutil.move(str(tmp), str(transcript))
        print(
            f"appended metadata footer hex={sentinel_hex} hash={sentinel_uuid} "
            f"to line {target}"
        )
        return 0
    except Exception as exc:
        print(f"error: {exc}", file=sys.stderr)
        if tmp.exists():
            tmp.unlink()
        return 1


if __name__ == "__main__":
    sys.exit(main())
```

## Appendix B: pass and fail matrix

Use this table to record verdicts for each fix-chain milestone. Today's column documents the current baseline; the fix-chain columns will be filled as each fix lands.

| Check | Today | After CLYDE-375 | After CLYDE-374 | After CLYDE-373 | After CLYDE-356 | After CLYDE-345 | After CLYDE-378 | After CLYDE-380 |
|-------|-------|---|---|---|---|---|---|---|
| Pre-Apply probe returns UUID1 | pass | pass | pass | pass | pass | pass | pass | pass |
| Post-Apply probe returns UUID1 | FAIL | FAIL | FAIL | unknown | FAIL | pass | pass | pass |
| Sentinel 2 hidden by boundary | pass | pass | pass | pass | pass | pass | pass | pass |
| Post-undo size matches pre-Apply | pass | pass | pass | pass | pass | pass | pass | pass |
| Post-undo sha matches pre-Apply | FAIL | pass | pass | pass | pass | pass | pass | pass |
| Snapshot file deleted after undo | FAIL | FAIL | pass | pass | pass | pass | pass | pass |
| Planner projection within 1 percent of `/context` | FAIL | FAIL | FAIL | pass | pass | pass | pass | pass |
| Apply refuses when projection over target | FAIL | FAIL | FAIL | FAIL | pass | pass | pass | pass |
| Wall clock under 90 seconds | FAIL | FAIL | FAIL | FAIL | FAIL | FAIL | pass | pass |
| Counter calls under 30 | FAIL | FAIL | FAIL | FAIL | FAIL | FAIL | pass | pass |
| ETA appears within 5 seconds | FAIL | FAIL | FAIL | FAIL | FAIL | FAIL | FAIL | pass |
| Step 10b: Second Apply lands with reduced chat-entry count | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a |
| Step 10b: Post-second-Apply probe still returns UUID1 | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a |
| Double-undo restores byte-identical pre-Apply state | n/a | n/a | n/a | n/a | n/a | n/a | n/a | n/a |

The three new rows above belong to the CLYDE-415, CLYDE-416, CLYDE-417 column once those fixes land. Until then they are not exercised by the linear fix-chain columns and read `n/a`. After the three Rehydrate or Dehydrate commits land, fill the column for that fix-chain milestone.

## Appendix C: related tickets

| Ticket | Concern |
|--------|---------|
| CLYDE-345 | Planner drops near-tail user messages under chat-only Apply |
| CLYDE-354 | Compact panel Progress region first row is iter 3 not iter 1 |
| CLYDE-356 | Apply mutates disk when final projection is over target |
| CLYDE-357 | Compact panel Context block shows dashes on form-open |
| CLYDE-358 | Dashboard `/` filter binding actually opens transcript search |
| CLYDE-359 | Dashboard `G` opens DETAIL pane instead of jumping to bottom |
| CLYDE-360 | Dashboard hides sessions whose basedir directory is missing |
| CLYDE-373 | Planner and resume-time counters disagree by 5 to 6 percent |
| CLYDE-374 | Undo leaves orphan snapshot files in backups directory |
| CLYDE-375 | Undo uses byte-offset truncation, corrupts on mid-file mutation |
| CLYDE-376 | Session list reorders rows while user is viewing |
| CLYDE-377 | Resume head-truncation claim, pending reinvestigation |
| CLYDE-378 | Planner makes one count_tokens call per dropped chat turn |
| CLYDE-380 | No ETA displayed during compaction Apply or Preview |
| CLYDE-381 | First-launch session list RPC times out after daemon reload |

## When to rerun this runbook

Run the procedure end-to-end any time you do one of these:

- Change anything in `internal/compact/`
- Change anything in `internal/contextusage/` or `internal/providers/claude/contextusage/`
- Change the dashboard's compact panel or its session list rendering
- Merge a daemon-ownership style refactor to main
- Cut a release

Preview-only runs (skip steps 6, 7, 8, 9, 11, 12) are appropriate for smaller daemon-side changes that should not affect the planner trajectory.
