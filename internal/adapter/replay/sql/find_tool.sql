WITH matches AS MATERIALIZED (
    SELECT conversation_id, reply_id, tool_digest
    FROM tool_replies
    WHERE tool_digest = ?1 AND (?2 = '' OR conversation_id = ?2)
    LIMIT 2
)
SELECT conversation_id, reply_id, provider, digest, visible_digest, tool_digest, blocks
FROM matches JOIN replies USING (conversation_id, reply_id)
WHERE (SELECT COUNT(*) FROM matches) = 1;
