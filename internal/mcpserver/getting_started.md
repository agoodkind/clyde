You have access to Clyde transcript tools for raw Claude and Codex conversations. Clyde reads provider-owned artifacts and never mutates them.

Available tools:

### clyde_search
Search, read, or browse Claude and Codex conversations. One operation with several modes, chosen from which inputs are set.
- query: text or semantic query to find in transcript messages.
- conversation: conversation id, native id, title, or artifact path to scope or read.
- around: message index to center a read window on; requires conversation.
- window: messages before and after for the read window and per-hit inline context.
- provider, workspace, roles, after, until, limit, min_score, include_archived: filters for discovery.

Modes:
- query alone searches the corpus. query plus conversation scopes the search to one conversation.
- conversation alone reads it: with around, a context window centered on that message index; otherwise the whole transcript.
- neither browses conversation metadata.

### clyde_reorient
Rebuild post-compaction recovery context as bounded, cursor-paged evidence, then resume the work. Use this after a fork and a compaction to recover the dropped detail.
- conversation: current conversation id. Empty uses the newest conversation in workspace.
- workspace: workspace root. Required when conversation is empty; also scopes memory and the fallback search.
- topic: narrows memory docs and the fallback search.
- cursor: continuation cursor from a prior page. Empty starts at the first page.
- window, limit, page_bytes, json: window size, evidence-item cap, page budget, and JSON output.

Each page carries a `remaining` count and a `next_cursor`. While `remaining` is above zero, call again with `cursor` set to `next_cursor`, and read every page, before reasoning.

### clyde_export_transcript
Export a conversation transcript.
- conversation_id (required): conversation id, native id, title, or artifact path.
- only (required): an array naming the content kinds to include. The export selects nothing by default, so name at least one kind.
- format (optional): markdown, html, json, or plain_text.
- whitespace (optional): preserve, tidy, compact, or dense.

Content kinds for the `only` array: `chat`, `thinking`, `tool_calls`, `tool_outputs`, `system_prompts`, `system_messages`, `raw_json_metadata`. Two group values fan out: `tools` covers `tool_calls` + `tool_outputs`, and `all` covers every kind. Example: `only: ["chat", "thinking", "tool_calls"]` or `only: ["all"]`. Tool outputs render only with `tool_outputs`, and raw metadata and provider system messages surface only with `format: "json"`.

Typical workflow:
1. Call clyde_search with a query to find where a topic was discussed.
2. Call clyde_search with a conversation and around to read the window around a useful message index.
3. Call clyde_reorient after a fork and compaction to rebuild the in-flight context, paging until remaining is zero.
4. Call clyde_export_transcript when you need a portable transcript.
