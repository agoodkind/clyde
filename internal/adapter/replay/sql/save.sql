INSERT INTO replies (conversation_id, reply_id, provider, digest, visible_digest, blocks)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (conversation_id, reply_id) DO UPDATE SET reply_id = excluded.reply_id
WHERE replies.provider = excluded.provider
  AND replies.digest = excluded.digest
  AND replies.visible_digest = excluded.visible_digest
  AND replies.blocks = excluded.blocks;
