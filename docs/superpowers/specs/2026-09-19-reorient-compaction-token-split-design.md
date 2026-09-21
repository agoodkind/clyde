# Reorient compaction split: select by token budget

Date: 2026-09-19
Status: Draft
Tickets: CLYDE-738, CLYDE-739, CLYDE-740, CLYDE-741, CLYDE-742

## Scope

The Claude MITM reorient split. The native Codex compaction path joined the
same split under CLYDE-742; see the Removed section.

## Selection

Current behavior: the split retains a fixed share of the messages by count,
`reorient_recent_fraction`, set to 0.5 by default and to 0.8 in the deployed
configuration. It forwards the remaining messages for summarization. It renders
the retained text from the session transcript file and caps that text at 3,500
lines.

Message size is unbounded and unrelated to message position. After a
compaction, message 1 stores the prior summary and the transcript injected
below it. A count-based selection assigns message 1 to the summarized remainder
every time. One session compacted three times on 2026-09-19 and retained
248,542 characters, then 1,685,220, then 57,234.

Required change:

1. The messages stay in their request form. No step renders the conversation
   into a single string.
2. The counter measures one message at a time and reads that message alone.
3. Clyde counts messages from the newest toward the oldest and sums those
   counts. Clyde retains every message that fits whole.
4. The boundary message is the first message that does not fit whole. Clyde
   stops counting there and retains the last part of the boundary message, up
   to the remaining budget.
5. The retained content measures strictly under the budget. It never equals the
   budget and never exceeds it.
6. The counter and the token settings are the ones
   `clyde conversation export --max-tokens` uses.
7. The count includes message 1. After one compaction, message 1 stores the
   prior summary and the prior injection and is the largest message in the
   request. A budget larger than every later message cuts inside message 1
   and retains its tail. When every message fits the budget, the upstream
   request keeps message 1 whole and clyde retains the rest, and the model
   summarizes message 1.
8. Clyde parses all content from the intercepted request. No compaction path
   reads a transcript file.
9. Clyde forwards the request unmodified, leaves the response unchanged, and
   logs `mitm.reorient_inject.split_fallback` with a reason when no content
   fits the budget.

## Truncation

Current behavior: the split removes whole messages and never divides one.

Required change: the cut divides the boundary message. The model summarizes the
first part of that message, and the injection carries the last part. No content
appears twice.

1. The upstream request retains every message older than the boundary message,
   plus the boundary message truncated at the cut.
2. The upstream request deletes every message newer than the boundary message.
3. Anthropic requires a result for every tool call in the same request. A cut
   inside a tool call deletes the matching result, and clyde appends a synthetic
   result for that call id in its place.
4. The synthetic result sets `is_error` to true, matching what Claude Code emits
   for the same case (claude-code source, `src/utils/messages.ts:5318-5325`).

### Example

Budget: 100 tokens. Message 1 is the oldest and message 6 is the newest.

| Message | Tokens | Running total |
| --- | --- | --- |
| 6 | 5 | 5 |
| 5 | 30 | 35 |
| 4 | 10 | 45 |
| 3 | 6 | 51 |
| 2 | 120 | 171 |

Messages 6 through 3 fit whole, at 51 tokens. Message 2 is the boundary
message, because 171 exceeds 100.

The retained content measures 99 tokens: the last 48 tokens of message 2, then
messages 3 through 6.

The upstream request retains message 1, then message 2 truncated 48 tokens
before its end. Messages 3 through 6 are deleted.

### Request JSON, before

Block shapes taken from capture row 361176. Real requests also interleave
`role: system` reminders between the assistant and user messages, and this
design treats them as ordinary messages.

```json
{
  "model": "claude-opus-5",
  "messages": [
    {"role": "user", "content": "start"},
    {"role": "assistant", "content": [
      {"type": "text", "text": "<120 tokens of file dump>"}]},
    {"role": "user", "content": "run the tests"},
    {"role": "assistant", "content": [
      {"type": "tool_use", "id": "toolu_01FT", "name": "Bash",
       "input": {"command": "go test ./..."}}]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_01FT",
       "content": "ok  42 tests"}]},
    {"role": "assistant", "content": [
      {"type": "text", "text": "all green"}]},
    {"role": "user", "content": [
      {"type": "text", "text": "Your task is to create a detailed summary..."}]}
  ]
}
```

### Request JSON, after

```json
{
  "model": "claude-opus-5",
  "messages": [
    {"role": "user", "content": "start"},
    {"role": "assistant", "content": [
      {"type": "text", "text": "<the first 72 tokens of the file dump>"}]},
    {"role": "user", "content": [
      {"type": "text", "text": "Your task is to create a detailed summary..."}]}
  ]
}
```

Message 2 is truncated at the cut. Messages 3 through 6 are deleted, including
the tool call `toolu_01FT` and the matching result, so this case needs no
synthetic result.

### Request JSON, a cut inside a tool call

The cut falls inside the `input` field of message 4, which makes message 4 the
boundary message. Message 5 is deleted, and a synthetic result takes its place:

```json
{
  "role": "user",
  "content": [
    {"type": "tool_result", "tool_use_id": "toolu_01FT",
     "content": "[Tool use was interrupted]", "is_error": true}
  ]
}
```

### Boundary message cases

| Boundary message content | Injected content | Upstream request |
| --- | --- | --- |
| A `text` block | The last part of that block | The block truncated at the cut |
| A `tool_result` content string | The last part of that string | The result truncated at the cut, beside the original call |
| A `tool_use` input | The last part of that call | The call truncated at the cut, plus a synthetic result for that call id |
| Every message fits the budget | Messages 2 onward | Message 1, then the compaction prompt |
| No tool calls anywhere | The last part of the boundary message | The boundary message truncated at the cut |

## Arguments

Current behavior: nothing reads the text typed after `/compact`.

Required change: `/compact` accepts the `clyde conversation export` argument
set. `--max-tokens` sets the budget, and the content arguments select which
blocks the counter measures and the injection includes.

1. Claude Code appends the text typed after `/compact` to the compaction
   prompt, below a line reading `Additional Instructions:` (claude-code source,
   `src/services/compact/prompt.ts:293-303`). That text is the argument source.
2. Three kinds of content appear in that block: declared arguments, free prose
   such as `focus on the test failures`, and instructions appended by PreCompact
   hooks. Hook instructions come after the operator's own text.
3. The parser consumes leading tokens that match the declared argument set and
   stops at the first token that does not match. The parser treats the remainder
   as content.
4. The upstream request retains the instruction region unmodified. The model
   receives the prose and the hook instructions whether or not arguments precede
   them.
5. `--max-tokens` accepts the human sizes `clyde conversation export` accepts,
   for example `500k`. Absent that argument, the budget is
   `reorient_inject_max_tokens`. Absent `--only` and every content shortcut,
   the content selection is `reorient_inject_content`, and an empty setting
   keeps every kind. A budget argument alone changes no content selection.
6. Argument parsing never fails a compaction. Clyde logs an unrecognized or
   invalid argument and proceeds on the configured budget.
7. Both commands read one parameter declaration. An argument added to
   `clyde conversation export` works on `/compact` with no further change.

## Package placement

Current behavior: `internal/reorientinject` sits directly under `internal/` and
declares `anthropicSummaryRequest`, `anthropicMessage`,
`compactPromptSignature`, and `summaryCloseTag`. The Generic Boundary contract
prohibits Anthropic wire shapes above a provider package, and it requires a
change touching a violating surface to move that knowledge to its provider home.

Required change:

1. `internal/adapter/anthropic` owns the Messages decode, the `tool_use` and
   `tool_result` block shapes, the truncation of one block at a byte offset, the
   synthetic result, and the `<summary>` span rewrite.
2. `internal/providers/claude` owns the compaction prompt signature, the
   `/compact` argument extraction, and the per-message token measurement.
3. `internal/reorientinject` declares the contract both packages implement. It
   imports no Anthropic type and constructs no Anthropic literal.
4. `internal/tokencount` and the `[export]` settings stay generic. They read no
   provider type.

## Removed

- `reorient_recent_fraction` and the bytes-per-token estimate, in the Claude
  path.
- The transcript-file content provider and its line cap.
- The deletion of `system` messages from the summarized remainder.
- Whole-request trim validation. Truncation plus the synthetic result keeps the
  request valid at the cut.

CLYDE-742 moved the native Codex compaction path onto the same split and
deleted `reorient_recent_fraction`, `reorient_bytes_per_token`,
`reorient_context_window_fraction`, and `reorient_standard_context_window`
from configuration. `reorient_summary_injection`, `reorient_inject_max_tokens`,
and `reorient_inject_content` remain, and both paths read them.
