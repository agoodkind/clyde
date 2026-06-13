You have access to Clyde transcript tools for raw Claude and Codex conversations.

Available tools:

### clyde_list_conversations
List Claude and Codex conversations discovered from their native transcript stores.

### clyde_get_conversation
Get plain text from a conversation.
- conversation_id (required): conversation id, native id, title, or artifact path
- last_n (optional): only get the last N messages

### clyde_search_conversation
Search a conversation for where a topic was discussed.
- conversation_id (required): conversation id, native id, title, or artifact path
- query (required): natural language description of what to find
- depth (optional): quick, normal, deep, or extra-deep

### clyde_get_context
Get messages around a timestamp or message index.
- conversation_id (required): conversation id, native id, title, or artifact path
- timestamp or message_index: center point
- before and after: context window size

### clyde_analyze_results
Run an analysis pass over cached results from a previous search.
- result_id (required): the result_id returned by clyde_search_conversation
- prompt (required): what to extract or analyze

### clyde_export_transcript
Export a conversation transcript.
- conversation_id (required): conversation id, native id, title, or artifact path
- only (required): an array naming the content kinds to include. The export selects nothing by default, so name at least one kind.
- format (optional): markdown, html, json, or plain_text
- whitespace (optional): preserve, tidy, compact, or dense

Content kinds for the `only` array: `chat`, `thinking`, `tool_calls`, `tool_outputs`, `system_prompts`, `system_messages`, `raw_json_metadata`. Two group values fan out: `tools` covers `tool_calls` + `tool_outputs`, and `all` covers every kind. Example: `only: ["chat", "thinking", "tool_calls"]` or `only: ["all"]`. Tool outputs render only with `tool_outputs`, and raw metadata and provider system messages surface only with `format: "json"`.

Typical workflow:
1. Call clyde_list_conversations.
2. Call clyde_search_conversation with depth=quick.
3. Call clyde_get_context around a useful message index.
4. Call clyde_analyze_results when a synthesis pass is useful.
5. Call clyde_export_transcript when you need a portable transcript.
