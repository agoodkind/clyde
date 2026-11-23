SELECT provider, digest, visible_digest, blocks
FROM replies
WHERE conversation_id = ? AND reply_id = ?;
