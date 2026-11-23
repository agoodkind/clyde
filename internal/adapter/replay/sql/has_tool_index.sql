SELECT EXISTS (
    SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'tool_replies'
);
