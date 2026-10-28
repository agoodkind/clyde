INSERT INTO retention_requests SELECT id FROM requests WHERE (? AND id<=?) OR (NOT ? AND (? OR (ts<? AND ?>0))) ORDER BY ts,id LIMIT CASE WHEN ? AND NOT ? THEN 1 ELSE 128 END
