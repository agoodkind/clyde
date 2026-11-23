CREATE TABLE IF NOT EXISTS replies (
    conversation_id TEXT NOT NULL,
    reply_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    digest TEXT NOT NULL,
    visible_digest TEXT NOT NULL,
    blocks BLOB NOT NULL,
    PRIMARY KEY (conversation_id, reply_id)
) WITHOUT ROWID;
