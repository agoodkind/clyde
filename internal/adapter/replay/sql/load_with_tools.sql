SELECT conversation_id, reply_id, provider, digest, visible_digest, COALESCE(tool_digest, ''), blocks
FROM replies LEFT JOIN tool_replies USING (conversation_id, reply_id)
WHERE conversation_id = ? AND reply_id = ?;
