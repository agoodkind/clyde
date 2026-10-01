UPDATE threads
SET data = CAST(replace(CAST(data AS TEXT), 'first', 'changed') AS BLOB),
    summary = summary || 'x'
WHERE id = 'context-windows';
