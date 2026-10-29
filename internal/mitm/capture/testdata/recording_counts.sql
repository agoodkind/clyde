SELECT
    (SELECT count(*) FROM requests),
    (SELECT count(*) FROM bodies),
    (SELECT count(*) FROM decoded_bodies),
    (SELECT count(*) FROM decoded_tool_events),
    (SELECT count(*) FROM drift_shapes),
    (SELECT count(*) FROM drift_checks);
