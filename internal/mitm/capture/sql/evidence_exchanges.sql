SELECT
	ingress.request_id,
	ingress_request.data,
	ingress_response.data,
	upstream_response.data
FROM requests AS ingress
JOIN requests AS upstream
	ON upstream.request_id = ingress.request_id
	AND upstream.client = ?
JOIN bodies AS ingress_request
	ON ingress_request.request_row_id = ingress.id
	AND ingress_request.which = 'request'
	AND ingress_request.truncated = 0
JOIN bodies AS ingress_response
	ON ingress_response.request_row_id = ingress.id
	AND ingress_response.which = 'response'
	AND ingress_response.truncated = 0
JOIN bodies AS upstream_response
	ON upstream_response.request_row_id = upstream.id
	AND upstream_response.which = 'response'
	AND upstream_response.truncated = 0
WHERE ingress.client = ?
	AND ingress.path = ?
	AND ingress.status = 200
	AND ingress.ts BETWEEN ? AND ?
	AND instr(ingress_response.data, CAST(? AS BLOB)) > 0
ORDER BY ingress.id
LIMIT ?;
