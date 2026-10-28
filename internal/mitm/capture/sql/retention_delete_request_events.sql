DELETE FROM decoded_tool_events WHERE decoded_body_id IN (SELECT id FROM decoded_bodies WHERE request_row_id IN (SELECT id FROM retention_requests))
