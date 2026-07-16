# Conversations

Clyde reads raw Claude and Codex conversation artifacts so users can inspect,
search, and export provider-owned transcripts without changing those provider
files.

## Model

Clyde treats each provider artifact as one conversation record. A record stores
metadata such as provider, native id, title, workspace root, artifact path,
artifact kind, model, creation time, update time, size, archive state, and
resolved fork lineage when the index can resolve it.

Clyde reads provider artifacts only. It does not create, rename, resume,
compact, wrap, or mutate Claude or Codex sessions.

## IDs

Conversation ids are derived ids:

- `claude:<provider-session-id>` for Claude transcripts with a provider session id.
- `codex:<thread-id>` for Codex transcripts with a thread id.
- `artifact:<path-hash>` when a provider id is not available.

CLI and MCP calls use `conversation_id` for this value. Most conversation tools
also accept a native id, title, or artifact path as a resolver input, but the
derived conversation id is the durable reference to store in notes, scripts, and
follow-up commands.

## Indexing

The daemon loads the last completed conversation cache at startup, then refreshes
conversation metadata in a debounced background worker. Refresh work is
idempotent by provider, artifact path, file size, and modified time.

A command can read stale cache data while a refresh is running. The refresh does
not change provider files.

## Reading

Use `clyde conversation info CONVERSATION_ID` before exporting when you need
static metadata or compaction segment numbers. The info command prints record
metadata, message counts, tool counts, compaction count, and the segment stack.

Use `clyde conversation show CONVERSATION_ID` when you only need plain text.
Use `clyde conversation context CONVERSATION_ID` when you need a window around a
message index or timestamp.

Use `clyde conversation search across` to search the indexed corpus. Use
`clyde conversation search within CONVERSATION_ID QUERY` to search one
conversation. Search returns excerpts and result ids; export is the portable
transcript path.

## Compaction Segments

A compaction segment is an exportable span in a conversation. When a segment has
a starting summary, the segment starts with that summary and includes the
messages after it until the next newer summary starts.

Segment numbers count from newest to oldest:

- Segment `0` is the latest compaction summary through the latest message.
- Segment `1` is the previous compaction summary through the message before
  segment `0`.
- Higher segment numbers continue toward older conversation history.
- The oldest segment has no starting summary when it begins at the start of the
  conversation.
- A conversation with no compaction has one segment, `0`, with no starting
  summary.

Export selectors count from newest to oldest. Export output remains
chronological.

For a conversation with two compactions, the chronological shape is:

```text
oldest messages | segment 2 | segment 1 summary and messages | segment 0 summary and latest messages
```

The segment stack in `conversation info` is printed newest first as `0`, `1`,
`2`, and so on. Each segment row shows whether it has a starting summary, its
start message index, its end message index, its summary timestamp when present,
its visible message count, its tool call count, and the export selector for that
segment.

Message indexes are zero-based. `start_message_index` is included in the
segment. `end_message_index` is excluded from the segment.

Visible message counts exclude compaction metadata messages and meta-only
messages.

## Export Selection

`clyde conversation export` includes segment `0` by default. This default keeps
the latest compaction summary and the messages after it.

Use `--include-compactions SELECTOR` to choose segments:

- `--include-compactions 0` exports the latest segment.
- `--include-compactions 0,1` exports segments `1` and `0`.
- `--include-compactions 0..2` exports segments `2`, `1`, and `0`.
- `--include-compactions all` exports every segment.

Use `--full-history` as readable sugar for `--include-compactions all`.

Unknown or out-of-range selectors fail with the available range, such as
`conversation has compaction segments 0..2`.

Use `--last-n N` to keep only the last `N` visible messages after segment
selection. When `--last-n` keeps messages from a segment that has a starting
summary, the export preserves that starting summary.

Use `--copy` on macOS to send the raw export body to `pbcopy`. On other
operating systems, the command returns a clear error instead of silently doing
nothing.

Use `clyde conversation export tail CONVERSATION_ID --last-n N` for a short
terminal transcript. Tail uses dense Markdown, includes `chat,tools`, selects
segment `0`, and writes the raw export body to stdout.

## Examples

Inspect static metadata and available segments:

```bash
clyde conversation info claude:1a2b3c
```

Export the latest compaction segment:

```bash
clyde conversation export claude:1a2b3c --only chat --stdout
```

Export the latest three segments and keep the last 50 visible messages:

```bash
clyde conversation export claude:1a2b3c --only chat,tools --include-compactions 0..2 --last-n 50 --stdout
```

Export every segment and copy the body on macOS:

```bash
clyde conversation export claude:1a2b3c --all --full-history --copy
```

Export a dense tail:

```bash
clyde conversation export tail claude:1a2b3c --last-n 20
```

## MCP

The MCP server uses the same operation registry as the CLI for conversation
operations. Use `clyde_conversation_info` to inspect metadata and the segment
stack. Use `clyde_export_transcript` to export transcript content.

`clyde_export_transcript` accepts `include_compactions`, `full_history`, and
`last_n` with the same segment selection rules as the CLI. The MCP tool writes
the export file into the caller working directory and returns its absolute path
in metadata. The text fallback retains the export body.

`conversation export tail` is a CLI-only shortcut. MCP clients can request the
same range by calling `clyde_export_transcript` with `include_compactions: "0"`,
`last_n`, `format: "markdown"`, `whitespace: "dense"`, and `only: ["chat",
"tools"]`.
