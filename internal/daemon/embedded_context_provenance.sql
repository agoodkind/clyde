SELECT source_path, source_stamp, provider
FROM batches
WHERE namespace = ? AND owner_id = ? AND generation_order = ? AND state = ?;
