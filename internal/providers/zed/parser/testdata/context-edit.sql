UPDATE threads
SET data = CAST(replace(CAST(data AS TEXT), 'original', 'changed') AS BLOB),
    summary = summary || 'x'
WHERE id = 'context-thread';
