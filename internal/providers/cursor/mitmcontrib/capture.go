package mitmcontrib

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/gklog/correlation"
)

const captureKind = "cursor_tls_http"

// CaptureExtension is the Cursor-owned capture.jsonl record emitted
// after an intercepted Cursor TLS request completes.
type CaptureExtension struct {
	Kind                string                `json:"kind"`
	T                   int64                 `json:"t"`
	TS                  string                `json:"ts"`
	Provider            string                `json:"provider"`
	ConcernName         string                `json:"concern"`
	Host                string                `json:"host"`
	Method              string                `json:"method"`
	Path                string                `json:"path"`
	Status              int                   `json:"status"`
	RequestBytes        int64                 `json:"request_bytes"`
	ResponseBytes       int64                 `json:"response_bytes"`
	RequestRawPath      string                `json:"request_raw_path"`
	ResponseRawPath     string                `json:"response_raw_path"`
	RequestID           string                `json:"request_id"`
	OriginalRequestID   string                `json:"original_request_id"`
	SessionID           string                `json:"session_id"`
	Traceparent         string                `json:"traceparent"`
	TraceID             string                `json:"trace_id,omitempty"`
	RequestContentType  string                `json:"request_content_type"`
	ResponseContentType string                `json:"response_content_type"`
	Diagnostic          *BidiAppendDiagnostic `json:"bidi_append,omitempty"`
	Hook                string                `json:"hook,omitempty"`
}

// Concern returns the provider-owned concern bucket for this capture
// extension.
func (e CaptureExtension) Concern() string {
	return e.ConcernName
}

// MarshalJSONLine returns the single JSONL record body for the capture
// extension.
func (e CaptureExtension) MarshalJSONLine() ([]byte, error) {
	type captureExtensionAlias CaptureExtension
	raw, err := json.Marshal(captureExtensionAlias(e))
	if err != nil {
		return nil, fmt.Errorf("marshal cursor capture extension: %w", err)
	}
	return raw, nil
}

func newCaptureExtension(exchange mitm.CaptureExchange) CaptureExtension {
	headers := ExtractCaptureHeaders(exchange.RequestHeader)
	traceID, _, hasTraceparent := correlation.ParseTraceparent(headers.Traceparent)
	concern := strings.TrimSpace(resolveCaptureConcern(exchange))
	capturedAt := exchange.CapturedAt.UTC()
	if exchange.CapturedAt.IsZero() {
		capturedAt = time.Unix(0, 0).UTC()
	}
	event := CaptureExtension{
		Kind:                captureKind,
		T:                   capturedAt.Unix(),
		TS:                  capturedAt.Format(time.RFC3339Nano),
		Provider:            ProviderName,
		ConcernName:         concern,
		Host:                exchange.Host,
		Method:              exchange.Method,
		Path:                exchange.Path,
		Status:              exchange.ResponseStatus,
		RequestBytes:        exchange.RequestBytes,
		ResponseBytes:       exchange.ResponseBytes,
		RequestRawPath:      exchange.RequestRawPath,
		ResponseRawPath:     exchange.ResponseRawPath,
		RequestID:           headers.RequestID,
		OriginalRequestID:   headers.OriginalRequestID,
		SessionID:           headers.SessionID,
		Traceparent:         headers.Traceparent,
		TraceID:             captureTraceID(traceID, hasTraceparent),
		RequestContentType:  contentType(exchange.RequestHeader, exchange.RequestContentType),
		ResponseContentType: contentType(exchange.ResponseHeader, exchange.ResponseContentType),
		Diagnostic:          nil,
		Hook:                exchange.HookName,
	}
	if exchange.HookName == "" {
		diag, ok := DiagnoseRequest(RequestCapture{
			Path:    exchange.Path,
			Headers: exchange.RequestHeader,
			Body:    exchange.DecodedRequestBody,
		}, nil)
		if ok {
			event.Diagnostic = &diag
		}
	}
	return event
}

func resolveCaptureConcern(exchange mitm.CaptureExchange) string {
	concern := strings.TrimSpace(exchange.Concern)
	if concern == "" {
		concern = "unknown"
	}
	return concern
}

func captureTraceID(traceID correlation.TraceID, ok bool) string {
	if !ok {
		return ""
	}
	return string(traceID)
}

func contentType(header http.Header, fallback string) string {
	if value := strings.TrimSpace(header.Get("Content-Type")); value != "" {
		return value
	}
	return fallback
}
