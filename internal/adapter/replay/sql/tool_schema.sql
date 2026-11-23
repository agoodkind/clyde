CREATE TABLE IF NOT EXISTS tool_replies (
    conversation_id TEXT NOT NULL,
    reply_id TEXT NOT NULL,
    tool_digest TEXT NOT NULL,
    PRIMARY KEY (conversation_id, reply_id)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS tool_replies_digest
ON tool_replies (tool_digest, conversation_id);
