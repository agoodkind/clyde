package mitmcontrib

import (
	"context"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/gklog/correlation"
)

// DiagnoseExchange implements the optional [mitm.ExchangeDiagnostician]
// contract. When the intercepted Cursor request looks like a BidiAppend
// gRPC-Web call, it decodes the typed [BidiAppendDiagnostic] from the
// decoded request body and emits it as a structured slog record on the
// MITM wire concern. The diagnostic rides the cursor wire concern log
// rather than the unified capture.jsonl leg: the unified capture record
// is transport-shaped and the BidiAppend protobuf decode is a
// cursor-only diagnostic, so it stays out of the shared leg facet. The
// decode never logs raw prompt bytes; only request id, append seqno,
// and content-length/hash summaries surface.
func (routeProvider) DiagnoseExchange(ctx context.Context, log *slog.Logger, exchange mitm.ExchangeDiagnostic) {
	if log == nil {
		return
	}
	if exchange.HookName != "" {
		return
	}
	diag, ok := DiagnoseRequest(RequestCapture{
		Path:    exchange.Path,
		Headers: exchange.RequestHeader,
		Body:    exchange.DecodedRequestBody,
	}, nil)
	if !ok {
		return
	}
	headers := ExtractCaptureHeaders(exchange.RequestHeader)
	traceID, _, hasTraceparent := correlation.ParseTraceparent(headers.Traceparent)
	log.InfoContext(ctx, "mitm.cursor.bidi_append.diagnostic",
		"component", "mitm",
		"concern", "providers.mitm.wire",
		"provider", ProviderName,
		"host", exchange.Host,
		"path", exchange.Path,
		"concern_bucket", resolveConcernBucket(exchange.Concern),
		"request_id", headers.RequestID,
		"original_request_id", headers.OriginalRequestID,
		"session_id", headers.SessionID,
		"trace_id", captureTraceID(traceID, hasTraceparent),
		"bidi_request_id", diag.RequestID,
		"bidi_append_seqno", diag.AppendSeqno,
		"bidi_decoded_bytes", diag.DecodedBytes,
		"bidi_decoded_sha256", diag.DecodedSHA256,
		"bidi_payload_bytes", diag.PayloadBytes,
		"bidi_payload_sha256", diag.PayloadSHA256,
		"bidi_sentinel_found", diag.SentinelFound,
		"bidi_sentinel_requested", diag.SentinelRequested,
	)
}

func resolveConcernBucket(concern string) string {
	concern = strings.TrimSpace(concern)
	if concern == "" {
		return "unknown"
	}
	return concern
}

func captureTraceID(traceID correlation.TraceID, ok bool) string {
	if !ok {
		return ""
	}
	return string(traceID)
}
