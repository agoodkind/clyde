# Compaction smoke runbook

This runbook is the acceptance gate for compaction. It defines what compaction
must do, the criteria a run must meet, where to start, and the rules a run must
follow. It is deliberately free of machine-specific commands, paths, session
ids, scripts, and per-run numbers; those belong in a transient run sheet that
the operator or agent writes for the specific machine and session they run
against.

Run this whenever you change planner code, the snapshot or undo path, the
context-usage probes, or the dashboard compact panel, and before cutting a
release.

## Goal

Compaction shortens an active session's transcript so the next resume reads a
smaller history. The planner reads the post-boundary slice, drops content along
four axes (thinking, images, tools, chat), condenses what it drops into one
summary, and appends one `compact_boundary` plus that summary in one append-only
write. The target is a floor: the committed result must be at or above the
target, as close to it as possible without going under. The full contract is in
[docs/compaction/algorithm.md](compaction/algorithm.md) under "The target
contract", and the resume-time on-chain mechanism is in the same file under
"Resume-time truncation". This runbook verifies the implementation honors that
contract end to end.

## What a run establishes

1. The planner preserves the most recent content. A canary placed on a near-tail
   message survives an aggressive compaction.
2. The boundary hides pre-boundary content. A canary placed before the boundary
   is not visible to the model on resume.
3. The compaction takes effect on resume. The boundary lies on the active
   `parentUuid` chain and claude's `/context` total drops on a fresh resume.
4. The result honors the target floor. The committed result is at or above
   target, as close as the enabled axes allow, never under.
5. The planner's projection matches reality. The projection is within one
   percent of claude's `/context`.
6. Apply is reversible and non-destructive. Apply only appends; undo restores the
   pre-apply file byte for byte and removes its snapshot.
7. A second apply still works. Compacting an already-compacted session rehydrates
   the prior summary and still preserves the survivor canary.

## Coverage

Run the procedure once per non-empty subset of the four stripper bits: the
fifteen combinations below. Each is an independent run that starts from a
pristine restore and ends with a restore, so no state leaks between runs. The
boundary-hide check and the second-apply check run once, on the all-strippers
run.

| Row | Strippers | clyde compact flags |
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

## Jumping-off points

- Build or choose a smoke session. You need a real session whose transcript is
  large enough to force the planner to work. Build one from scratch by running a
  session and filling it, or reuse an existing large session. Record its session
  id, working directory, and transcript path in your run sheet.
- A row can only drop the content its axes govern. A session that is almost all
  tool output exercises the tools axis but leaves chat-only, thinking-only, and
  image-only rows as no-ops. Choose a session, or per-row targets, so each row's
  enabled axes have real content to drop. Record an axis-empty row as
  floor-above-target, not as a planner fault.
- Pick each row's target below the session's pre-apply `/context` total by
  enough to force drops. A target above the current total is accepted with zero
  drops and exercises nothing.
- Before the large target, do a quick dashboard sanity pass on a small session:
  resume it, send a message, exit to the return prompt, and open the compact
  panel from there. This confirms session listing, navigation, and the panel
  before any mutation.

## Canary methodology

Use the metadata-footer canary. Embed a fake skill metadata footer, a short hex
skill id and a UUID build hash, inside a real message, then ask the model "what
is the build hash for skill X". The model treats the footer as a fact it can
describe: it returns the UUID when the content is in its context and says it has
no information about the skill when the content is gone. The answer is
deterministic across phrasings.

Do not use instruction-style canary text such as "WHEN THE USER ASKS X OUTPUT
Y". The model reads that as a prompt-injection attempt and refuses to repeat the
UUID, which is a false negative even when the content is present.

A "not found" from a plain "find the UUID starting with X" prompt is not
trustworthy, because the model can miss content that is in its context and can
fabricate content that is not. Only the metadata-footer form is an acceptable
oracle.

## Tooling and helpers

The procedure drives compaction through the `clyde compact` CLI for preview,
apply, undo, and the backup ledger, and through claude print mode (`claude -p`)
for the canary and `/context` probes. Drive the dashboard compact panel with
`tmux` for the TUI smoke so the keystrokes are scriptable.

A run needs three helpers, each held to its requirement:

- A canary splicer: given a transcript, a line, a hex skill id, and a UUID, it
  appends the metadata footer to that line's text content and rewrites the file
  atomically, refusing a line whose first content block is not text.
- A chain checker: given a transcript, it rebuilds the active message list by
  following `parentUuid` from the last message to the root and reports, for a
  boundary, whether the boundary's `parentUuid` is set and whether it lies on
  that chain, plus the file-order-last uuid a new boundary would attach to.
- A `/context` reader: given the JSON from a `/context` probe, it extracts the
  Messages token count and the total.

## Strict guidelines

- Run every check in order. Do not skip a check because you can predict its
  result. If you miss a step, restart the run from the beginning.
- Back up the transcript before any apply, and restore from that backup at the
  end of every run. Confirm no canary strings remain in any transcript except
  the chat you are running from.
- Every non-chat use of `claude` (reading a session, asking about its context,
  capturing `/context`) passes `--no-session-persistence` and never passes
  `--fork-session`, so the probe writes nothing and cannot contaminate the run.
- Capture `/context` alongside every canary probe. The `/context` reading is the
  upstream truth the planner's projection is compared against.
- Disable tools on every probe so the model cannot read the transcript from disk
  and bypass the test.
- Use the smallest model that fits the session's context window. A session under
  roughly 200k tokens uses the haiku model; reserve the opus 1M model for
  sessions that genuinely need the 1M window.
- When you find a bug: investigate the root cause, fix it, add a test, run the
  full make checks, deploy, then restart the run from the beginning. Each bug fix
  is its own commit. Stay on `main`. Never bypass the pre-commit hook. Never edit
  lint baseline files.

## Acceptance criteria

A run passes only when every check below passes for every coverage row that has
content on its axes. Record each result in your run sheet.

- Pre-apply: the model returns the survivor canary. If it does not, the
  methodology is broken on this machine; stop before applying.
- Post-apply: the model still returns the survivor canary. The planner preserved
  the most recent content.
- The new boundary's `parentUuid` is set, and the boundary lies on the active
  `parentUuid` chain.
- Claude's `/context` total drops on a fresh resume below the pre-apply total.
  This is the decisive proof the compaction took effect.
- The committed result is at or above target and as close to it as the enabled
  axes allow. A result under target, meaning over-trimmed, fails.
- The planner's projection is within one percent of the `/context` reading.
- The boundary hides the pre-boundary canary: the model has no information about
  it on resume.
- Undo restores the pre-apply file byte for byte and removes its snapshot.
- A second apply on the compacted session lands a smaller result and still
  returns the survivor canary.

## When to rerun

Run the procedure end to end any time you do one of these:

- Change anything in `internal/compact/`.
- Change anything in `internal/contextusage/` or
  `internal/providers/claude/contextusage/`.
- Change the dashboard's compact panel or its session list rendering.
- Merge a daemon-ownership style refactor to `main`.
- Cut a release.
