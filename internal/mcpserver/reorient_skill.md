# Reorient

`/compact` replaces the live conversation with a summary. The summary keeps the shape of the work and drops the detail: exact file paths, prior decisions, tool results, and the step that was in progress. Reorient rebuilds that detail from two sources that still hold it in full, the recorded conversation transcript and the codebase, states where the work stands, and continues from the next concrete step.

Run this workflow when the user types "reorient" or "/reorient". Focus on the most recent task in the current session.

## Steps

1. Recover the prior conversation through clyde, the local server that indexes Claude and Codex transcripts and serves them as MCP tools. Call `clyde_reorient` with the current conversation id, or with the workspace root to start from its newest conversation. The tool returns the conversation history from before the compaction point as a dense chat plus tool-call transcript, delivered as bounded cursor-paged text. Each page carries a `remaining` byte count and a `next_cursor`. While `remaining` is above zero you have not seen all of it: call `clyde_reorient` again with the same inputs and `cursor` set to the printed `next_cursor`, and keep going until `remaining` is zero. Read every page in full, and do not grep the pages. If a page reports `restart`, the conversation was compacted again while you paged, so start over with an empty cursor. Use `clyde_search` for a deeper windowed read of any conversation the recovered transcript points to, with `conversation_id: "<id>"`, `around: <index>`, and `window: <n>`. Treat the compaction summary already in your context as an index into this recovered transcript, not as the source of truth.

2. Discover code through the semantic MCP server, the local server that indexes the repository for semantic code search, rather than through grep. Check `get_indexing_status` on the absolute repository root first, ask the user before indexing and wait for an explicit yes, prefer `search_code` with the source-language extension filter, and fall back to grep or ripgrep only after one corrective pass still returns documentation or generated files.

3. State the reorientation brief: the task, what is done, what is in flight, the exact files and symbols in play, and the next concrete step. Each line stands on its own when read offline.

4. Resume the work from the next concrete step.

## Critical rules

- Produce the brief and the continued work only. No apology, no preface, no acknowledgment of the command.
- Exhaust the recovered transcript before you reason. Keep paging until `remaining` is zero, and read every page in full rather than grepping it.
- Recover every fact from the recovered transcript and the code. Do not reconstruct facts from the compaction summary.
- Semantic search through the semantic MCP server precedes grep for conceptual discovery.
- Every line of output reads correctly offline, with each symbol, path, and reference defined inline.
- State how the work stands today. Do not narrate the compaction or what the context held before it.
