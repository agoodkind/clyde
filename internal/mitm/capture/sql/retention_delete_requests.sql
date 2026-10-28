DELETE FROM requests WHERE id IN (SELECT id FROM retention_requests)
