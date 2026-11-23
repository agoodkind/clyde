INSERT INTO tool_replies (conversation_id, reply_id, tool_digest)
VALUES (?, ?, ?)
ON CONFLICT (conversation_id, reply_id) DO UPDATE SET reply_id = excluded.reply_id
WHERE tool_replies.tool_digest = excluded.tool_digest;
