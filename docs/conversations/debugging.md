# Conversation ingest debugging

This guide inventories every observable surface in the conversation ingest
pipeline, states what question each surface answers, and names the traps that
make its readings lie. It teaches where to look and how to interpret, not one
fixed procedure. Commands shown are worked examples, not the method.

The pipeline: provider artifacts → clyde parser and index → feeder projection →
engine needed-set and jobs → embedding host → Milvus rows. A conclusion drawn
from one surface is a hypothesis until a second surface agrees.

**The store is append-only, ever.** A re-ingest appends new rows beside the old
ones; it never replaces, corrects, or re-numbers anything, and stored rows
never converge toward current behavior. Never recommend or design around a
self-heal re-index: for a finished conversation the re-ingest never comes, and
even when one happens the old rows remain exactly as written. A fix for how
stored data is interpreted must make old rows correct as they are.

## Method

Apply these to every reading, on every surface.

- Tier every claim: verified, inferred, or assumed. Only verified claims close a
  question.
- Verify the instrument before the reading. Inject a known value and watch it
  appear. An uncalibrated counter may include work you did not expect or filter
  on fields that do not exist.
- Measure rates from two timed samples, never one. A single sample cannot
  distinguish throughput from a snapshot.
- Keep a control beside every effect: measure something that must not change,
  or the effect cannot be distinguished from noise.
- Check log rotation before trusting an absence. A missing event may be in a
  rotated `.gz` file, not missing.
- Record numbers with their query so a later audit can compare them.

## Clyde surfaces

### Config

`[conversation]` and `[conversation.semantic]` in the clyde config own
`enabled`, `search_enabled`, `indexed_content`, `collection_id`, `socket_path`,
and `include_subagent_conversations`. See
[reload and hot apply](../reload-and-hot-apply.md) for which fields apply in
process and which need a reload.

- A config edit is not a deploy. The watcher classifies the change; confirm the
  running daemon holds the value by reading the config-applied events in the
  daemon log, not by reading the file.
- `enabled = false` stops new embedding only. Search keeps answering from the
  existing corpus, so a working search proves nothing about the feeder.
- An empty `indexed_content` means the default kind set (chat plus tool calls),
  not "index nothing".

### Running build identity

`clyde --version` names the running build; compare it against the repo tip
before trusting any live measurement.

- One machine-global daemon serves every worktree. A deploy from another
  worktree replaces the running build mid-measurement, so re-check the version
  after any long observation window.
- The version string embeds the commit; a build that predates your change makes
  every downstream reading meaningless.

### Conversation index

The index answers "does clyde see this conversation, and how is it
classified".

- The daemon loads the last completed cache at startup and refreshes in a
  debounced background worker, so a fresh artifact can lag in listings without
  anything being wrong.
- Cached records re-derive only when the artifact stamp changes or
  `cacheFormatVersion` is raised. A parser classification change without a
  version bump leaves stale classifications in place indefinitely.
- Origin classification separates user conversations from subagent machinery;
  the skip counters in the daemon log say how many were withheld and why.
- Duplicate-artifact id collisions resolve to one record; a conversation you
  cannot find may be hiding behind another artifact with the same derived id.

### Rendering and projection

Rendering turns messages into the documents the engine stores. The resolved
content-kind set (the same selector vocabulary export uses) decides what text
survives.

- Document families are `conv/` (chat), `convtool/` (tool calls), and
  `convthink/` (thinking); the stored `relativePath` is the synthetic
  `<family>/<conversationId>/<messageIndex>`, so path substring probes against
  real filesystem paths match nothing. Query by `conversationId`.
- `MessageIndex` is a position fed back into the loader by search. Skipped
  messages leave gaps; nothing renumbers. An index loaded under a different
  kind set can point at a different message (CLYDE-638).
- Any rendering change alters chunk bytes, and reuse is keyed on exact bytes,
  so a conversation that changes after a rendering change re-embeds in full.
  This is the mechanism behind "why is the engine re-embedding everything". The
  new rows append beside the old ones per the append-only contract above; a
  rendering change never corrects anything already stored.

### Feeder

The feeder log is `logs/conversation/semantic.jsonl` under the clyde state
directory. Each `pass_completed` line reports one pass.

- `manifest` counts advertised conversations; `needed` counts what the engine
  asked for; `documents` counts projected documents. None of these measure
  tokens, so none predict embedding time.
- The manifest fingerprint is size plus mtime only. Content does not enter it,
  so a touch without a byte change re-offers a conversation, and a deploy alone
  re-offers nothing.
- `injected_stripped` and `system_stripped` undercount: the parser drops fully
  harness-written records before the projection counts them, so near-zero
  values do not mean stripping is off (CLYDE-639). Prove exclusion with a
  differential run instead: project the same transcripts under the live kind
  set and under an opted-in set, and diff the offered text.
- Two suppression maps keep the manifest honest: zero-document conversations
  and failed loads are omitted until their artifact changes, and
  `failed_suppressed` reports the latter. A silently shrinking manifest reads
  as a drained corpus.
- The wire contract is additive-only: absence from a manifest retains stored
  rows. Nothing the feeder does deletes.
- The pass log carries no conversation ids at info level, so attributing a pass
  to specific conversations needs debug logging or an engine-side query
  (CLYDE-640).

### Clyde logs

`clyde-daemon.jsonl` and the per-concern files under `logs/` in the clyde state
directory carry `trace_id` and `request_id`, which join across the daemon, the
feeder, and engine RPCs. Raise one concern to debug rather than the whole
daemon when per-conversation detail is needed.

## Engine surfaces (lm-semantic-search)

### Job system

`lm-semantic-search job list` and `job get` answer "what is the engine doing
and how far along is it".

- Terminal states are completed, failed, superseded, and canceled. A superseded
  job is not a failure; a newer trigger replaced it.
- `overall_percent` counts whole documents. One giant in-flight document
  freezes the percent while the GPU runs at full throughput, so a flat percent
  is not a stall.
- Chunk density varies more than thirtyfold across documents, so chunks per
  minute swings at constant GPU throughput. Measure ingest with span durations,
  and lead with the max, not the median.

### Engine logs

The engine writes per-concern files (`semantic`, `indexer`, `daemon`,
`converge`, `watcher`, `job`) under its state `logs/` directory, with rotated
`.gz` siblings.

- `embed_batch_started` carries input counts and `estimated_tokens`, the only
  density signal.
- `chunks_written` attributes output per document.
- `embed_input_dropped` names each rejected input and why; a per-input
  rejection is skip-and-log, never a global pause.
- The `daemon.rpc.*` events carry method names only, no conversation ids.

### Embedding host (lmd)

lmd owns the GPU. Its stream lives in the macOS unified log under subsystem
`io.goodkind.lmd`; use the absolute `/usr/bin/log` because zsh shadows `log`.

- GPU utilization is the ground truth for "is embedding progressing"; every
  higher-level counter can freeze while the GPU works.
- The token cap converts at 2.5 bytes per token, so dense content splits into
  sub-chunks around 9,215 bytes; a document can therefore produce more inputs
  than messages.

### Engine state

The registry, merkle checkpoints, chunk caches, and locks live under the
engine state directory.

- A model-name or config-digest change forces a full re-embed; the checkpoint
  cannot vouch for vectors made under a different config.
- Never run the engine binary as a probe against live state: a probe run can
  wipe the checkpoint. Read state files; do not exercise them.
- A stale lock whose owner pid was reused blocks silently; verify lock owners
  are alive before trusting "busy".

### Milvus

The vector store answers "what is actually retrievable". Query it directly;
no higher surface can prove a row exists.

- Discover collections including staging (`_stg`) suffixes; a promote swaps
  staging in, so a count taken mid-promote misleads.
- Rows carry no insert timestamp. Attribute recent writes by diffing a
  snapshot taken before the window against one taken after, keyed by
  `messageIndex` plus `contentHash`.
- The era split is `contentHash IS NULL` (legacy rows) versus set (current
  rows). Both eras coexist in one conversation because the store is
  append-only.
- Page with limit and offset; a bare query truncates silently at the default
  limit.
- Marker text in a stored row is not proof of an ingest leak: conversations
  legitimately quote markers in user-typed and assistant text. Classify each
  hit by role and context before calling it harness content.

### Reuse

Vector reuse is keyed on the sha256 of exact chunk bytes and crosses corpora.
Reuse makes re-offers of unchanged content cheap, and rendering changes defeat
it wholesale. `reuse_vectors_loaded` in the engine logs says how much of a job
was reuse rather than fresh embedding.

### Search read path

The needed set is the engine's diff of the offered manifest against its
checkpoint (`SyncConversationManifest`), capped per ingest with a rotating
cursor, so a large backlog drains across passes by design. Reconcile is
retain-mode: absence never deletes. Read-side failures such as
`context_length_exceeded` and `source_unavailable` surface during search and
look like ingest problems; they are not.

## Provider artifacts

Ground truth is the artifact, and each provider shapes it differently.

- Claude: one JSONL per session under `~/.claude/projects/<encoded-cwd>/`,
  with subagent transcripts under the per-session directory
  `<encoded-cwd>/<session-id>/subagents/`. Records carry `type`
  (`user`, `assistant`, `system`, `attachment`, `queue-operation`) and the
  visibility flags `isMeta`, `isVisibleInTranscriptOnly`, and
  `isCompactSummary`. Hook-injected rule text rides in records typed `user`
  and in `attachment` records, so no system-message knob removes it; the
  injected content kind does.
- Codex: rollout JSONL under `~/.codex/sessions/<year>/<month>/<day>/`. Harness
  frames (AGENTS.md instructions, environment context, approval and
  transcript-delta frames) are written into user-role records and classify by
  head-anchored match.
- Cursor: two stores plus agent transcript files, none complete alone; see
  [Cursor stores](../cursor/stores.md). Subagent transcripts are written
  twice, and the top-level twin carries no marker.
- Zed: threads in an application-owned SQLite database, opened read-only.
- Time bases differ per surface: transcript timestamps are UTC, log lines are
  local time with offsets, and the MITM capture store keeps nanoseconds.
  Normalize before joining.

## Worked examples

- Differential kind-set projection: build a small binary at the deployed
  commit that calls `SemanticConversationLoadOptions` and
  `BuildSemanticConversationDocuments` over chosen transcripts twice, once
  with the live kinds and once with the harness kinds opted in, then diff
  marker counts. This is the instrument that proves feed exclusion when the
  pass counters cannot.
- Store attribution: snapshot a conversation's rows before an ingest window,
  re-query after, and classify only the new keys. This separates historical
  rows from what the current build wrote.
- Rate measurement: read a counter, wait a fixed interval, read it again, and
  report the delta over the interval alongside what did not change.
