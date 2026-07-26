CREATE TABLE IF NOT EXISTS requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	client TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	concern TEXT NOT NULL DEFAULT '',
	host TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL DEFAULT 0,
	request_id TEXT NOT NULL DEFAULT '',
	upstream_request_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	trace_id TEXT NOT NULL DEFAULT '',
	req_headers TEXT NOT NULL DEFAULT '',
	resp_headers TEXT NOT NULL DEFAULT '',
	req_content_type TEXT NOT NULL DEFAULT '',
	resp_content_type TEXT NOT NULL DEFAULT '',
	req_bytes INTEGER NOT NULL DEFAULT 0,
	resp_bytes INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	-- conversation_id is clyde's derived conversation id for the chat this
	-- request belongs to, empty when the request names no chat. Databases
	-- created before these columns existed pick them up through
	-- ensureRequestConversationColumns, which also creates their index; the
	-- index cannot live here because this file runs before that ALTER.
	conversation_id TEXT NOT NULL DEFAULT '',
	conversation_source TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_client ON requests(client);
CREATE INDEX IF NOT EXISTS idx_requests_host ON requests(host);
CREATE INDEX IF NOT EXISTS idx_requests_concern ON requests(concern);
CREATE INDEX IF NOT EXISTS idx_requests_request_id ON requests(request_id);
CREATE INDEX IF NOT EXISTS idx_requests_trace_id ON requests(trace_id);
CREATE TABLE IF NOT EXISTS bodies (
	request_row_id INTEGER NOT NULL,
	which TEXT NOT NULL,
	content_type TEXT NOT NULL DEFAULT '',
	is_text INTEGER NOT NULL DEFAULT 0,
	truncated INTEGER NOT NULL DEFAULT 0,
	data BLOB,
	PRIMARY KEY (request_row_id, which)
);
CREATE TABLE IF NOT EXISTS decoded_bodies (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_row_id INTEGER NOT NULL,
	which TEXT NOT NULL,
	format TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	decode_error TEXT NOT NULL DEFAULT '',
	representation_json BLOB NOT NULL DEFAULT '',
	UNIQUE(request_row_id, which)
);
CREATE INDEX IF NOT EXISTS idx_decoded_bodies_request ON decoded_bodies(request_row_id);
CREATE TABLE IF NOT EXISTS decoded_tool_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	decoded_body_id INTEGER NOT NULL,
	ordering INTEGER NOT NULL DEFAULT 0,
	ordering_text TEXT NOT NULL DEFAULT '',
	call_id TEXT NOT NULL DEFAULT '',
	tool_name TEXT NOT NULL DEFAULT '',
	input_representation TEXT NOT NULL DEFAULT '',
	input_encoding TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_decoded_tool_events_body ON decoded_tool_events(decoded_body_id, ordering);
CREATE TABLE IF NOT EXISTS drift_shapes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	provider TEXT NOT NULL DEFAULT '',
	upstream TEXT NOT NULL DEFAULT '',
	fingerprint TEXT NOT NULL DEFAULT '',
	first_seen INTEGER NOT NULL DEFAULT 0,
	last_seen INTEGER NOT NULL DEFAULT 0,
	seen_count INTEGER NOT NULL DEFAULT 0,
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	url TEXT NOT NULL DEFAULT '',
	request_headers TEXT NOT NULL DEFAULT '',
	request_body TEXT NOT NULL DEFAULT '',
	billing_attestation TEXT NOT NULL DEFAULT '',
	request_features TEXT NOT NULL DEFAULT '',
	UNIQUE(upstream, fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_drift_shapes_upstream ON drift_shapes(upstream, last_seen);
CREATE TABLE IF NOT EXISTS baselines (
	upstream TEXT PRIMARY KEY,
	snapshot TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS baseline_diffs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	upstream TEXT NOT NULL DEFAULT '',
	flavor TEXT NOT NULL DEFAULT '',
	category TEXT NOT NULL DEFAULT '',
	field TEXT NOT NULL DEFAULT '',
	before_value TEXT NOT NULL DEFAULT '',
	after_value TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	first_seen INTEGER NOT NULL DEFAULT 0,
	last_seen INTEGER NOT NULL DEFAULT 0,
	seen_count INTEGER NOT NULL DEFAULT 0,
	UNIQUE(upstream, flavor, category, field, before_value, after_value)
);
CREATE INDEX IF NOT EXISTS idx_baseline_diffs_upstream ON baseline_diffs(upstream);
CREATE TABLE IF NOT EXISTS drift_checks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	upstream TEXT NOT NULL DEFAULT '',
	ts INTEGER NOT NULL DEFAULT 0,
	diverged INTEGER NOT NULL DEFAULT 0,
	summary TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_drift_checks_upstream ON drift_checks(upstream, ts);
