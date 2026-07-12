package mitmcontrib

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitm/capture"
)

func TestRouteProviderClaimsCursorConnectHosts(t *testing.T) {
	t.Parallel()

	provider := routeProvider{}
	claim := provider.ClassifyConnect("api2.cursor.sh")
	if !claim.Claimed {
		t.Fatal("expected api2.cursor.sh claim")
	}
	if claim.ProviderID != providerID {
		t.Fatalf("provider id = %v want %v", claim.ProviderID, providerID)
	}
	if provider.ClassifyConnect("api.openai.com").Claimed {
		t.Fatal("expected non-Cursor host to fall through")
	}
}

func TestRouteProviderExtractsCursorIdentity(t *testing.T) {
	t.Parallel()

	headers := http.Header{}
	headers.Set("x-request-id", "cursor-req")
	headers.Set("x-original-request-id", "cursor-orig")
	headers.Set("x-session-id", "cursor-session")
	headers.Set("x-cursor-conversation-id", "cursor-conv")
	contrib := routeProvider{}.ExtractIdentity(headers)
	if contrib.PreferredRequestID != "cursor-req" {
		t.Fatalf("preferred request id = %q", contrib.PreferredRequestID)
	}
	if contrib.PreferredUpstreamRequestID != "cursor-orig" {
		t.Fatalf("preferred upstream request id = %q", contrib.PreferredUpstreamRequestID)
	}
	if contrib.SessionID != "cursor-session" {
		t.Fatalf("session id = %q", contrib.SessionID)
	}
	if contrib.Facet == nil {
		t.Fatal("expected Cursor facet")
	}
}

func TestDiagnoseExchangeEmitsBidiDiagnosticOnWireConcern(t *testing.T) {
	t.Parallel()

	const requestID = "req_cursor_bidi_append"
	const sentinel = "CURSOR_PROMPT_SENTINEL"
	headers := http.Header{}
	headers.Set("content-type", "application/protobuf")
	headers.Set("x-request-id", requestID)
	headers.Set("x-original-request-id", "orig-123")
	headers.Set("x-session-id", "sess-123")
	headers.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	body := cursorBidiAppendPayload(requestID, 42, []byte("before "+sentinel+" after"))

	logBuffer := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	routeProvider{}.DiagnoseExchange(context.Background(), logger, mitm.ExchangeDiagnostic{
		RequestHeader:      headers,
		DecodedRequestBody: body,
		Method:             http.MethodPost,
		Path:               "/aiserver.v1.AiService/BidiAppend",
		Host:               "api2.cursor.sh",
		Concern:            "cursor.bidi",
		HookName:           "",
	})

	raw := logBuffer.Bytes()
	if bytes.Contains(raw, []byte(sentinel)) {
		t.Fatalf("diagnostic log leaked sentinel: %s", raw)
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &event); err != nil {
		t.Fatalf("unmarshal diagnostic log: %v\n%s", err, raw)
	}
	if event["msg"] != "mitm.cursor.bidi_append.diagnostic" {
		t.Fatalf("msg = %v want mitm.cursor.bidi_append.diagnostic", event["msg"])
	}
	if event["concern"] != "providers.mitm.wire" {
		t.Fatalf("concern = %v want providers.mitm.wire", event["concern"])
	}
	if event["provider"] != ProviderName {
		t.Fatalf("provider = %v want %q", event["provider"], ProviderName)
	}
	if event["trace_id"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("trace id = %v", event["trace_id"])
	}
	if event["bidi_request_id"] != requestID {
		t.Fatalf("bidi request id = %v want %q", event["bidi_request_id"], requestID)
	}
	if event["bidi_append_seqno"].(float64) != 42 {
		t.Fatalf("bidi append seqno = %v want 42", event["bidi_append_seqno"])
	}
}

func TestDiagnoseExchangeSkipsNonBidiRequests(t *testing.T) {
	t.Parallel()

	logBuffer := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	routeProvider{}.DiagnoseExchange(context.Background(), logger, mitm.ExchangeDiagnostic{
		RequestHeader:      http.Header{},
		DecodedRequestBody: []byte("not a bidi frame"),
		Method:             http.MethodPost,
		Path:               "/aiserver.v1.DashboardService/GetTeams",
		Host:               "api2.cursor.sh",
		Concern:            "cursor.account",
		HookName:           "",
	})
	if logBuffer.Len() != 0 {
		t.Fatalf("expected no diagnostic for non-bidi request, got %s", logBuffer.String())
	}
}

func TestDecodeCaptureRequestPreservesFieldTreeAndToolEvent(t *testing.T) {
	outer := cursorBidiAppendPayload("request-1", 4, []byte("0a0663616c6c2d311209656469745f66696c651a117b227061746368223a2268656c6c6f227d"))

	decoded, ok := DecodeCaptureRequest(RequestCapture{
		Path:    "/aiserver.v1.AiService/BidiAppend",
		Headers: http.Header{"Content-Type": []string{"application/protobuf"}},
		Body:    outer,
	})
	if !ok {
		t.Fatal("expected BidiAppend capture decode")
	}
	if decoded.Status != capture.DecodeStatusSuccess {
		t.Fatalf("status = %q error = %q", decoded.Status, decoded.Error)
	}
	if !bytes.Contains(decoded.RepresentationJSON, []byte(`"hex_envelope"`)) {
		t.Fatalf("representation omits hex envelope: %s", decoded.RepresentationJSON)
	}
	if len(decoded.ToolEvents) != 1 {
		t.Fatalf("tool events = %d", len(decoded.ToolEvents))
	}
	event := decoded.ToolEvents[0]
	if event.Ordering != 4 || event.CallID != "call-1" || event.ToolName != "edit_file" || event.InputRepresentation != `{"patch":"hello"}` || event.InputEncoding != capture.ToolInputEncodingJSON {
		t.Fatalf("tool event = %#v", event)
	}
}

func TestDecodeCaptureRequestMarksRawToolInputAsBase64(t *testing.T) {
	payload := appendProtoString(nil, 1, "call-raw")
	payload = appendProtoString(payload, 2, "apply_patch")
	payload = appendProtoBytes(payload, 3, []byte{0xff, 0x00, 0x7f})
	outer := cursorBidiAppendPayload("request-raw", 5, []byte(hex.EncodeToString(payload)))

	decoded, ok := DecodeCaptureRequest(RequestCapture{
		Path: "/aiserver.v1.AiService/BidiAppend",
		Body: outer,
	})
	if !ok || len(decoded.ToolEvents) != 1 {
		t.Fatalf("decoded=%#v ok=%t", decoded, ok)
	}
	event := decoded.ToolEvents[0]
	if event.InputEncoding != capture.ToolInputEncodingBase64 {
		t.Fatalf("input encoding=%q", event.InputEncoding)
	}
	if event.InputRepresentation != "/wB/" {
		t.Fatalf("input representation=%q", event.InputRepresentation)
	}
}

func TestDecodeCaptureRequestPersistsMalformedFrameFailure(t *testing.T) {
	decoded, ok := DecodeCaptureRequest(RequestCapture{
		Path: "/aiserver.v1.AiService/BidiAppend",
		Body: []byte{0x0a, 0x80},
	})
	if !ok {
		t.Fatal("expected BidiAppend capture decode")
	}
	if decoded.Status != capture.DecodeStatusFailed {
		t.Fatalf("status = %q", decoded.Status)
	}
	if decoded.Error == "" {
		t.Fatal("decode error is empty")
	}
}

func TestDecodeCaptureRequestUsesSequenceFieldForToolOrdering(t *testing.T) {
	payload := []byte("0a0663616c6c2d311209656469745f66696c651a117b227061746368223a2268656c6c6f227d")
	outer := appendProtoString(nil, 1, "request-1")
	outer = appendProtoVarint(outer, 9, 99)
	outer = appendProtoVarint(outer, 2, 4)
	outer = appendProtoBytes(outer, 3, payload)

	decoded, ok := DecodeCaptureRequest(RequestCapture{
		Path: "/aiserver.v1.AiService/BidiAppend",
		Body: outer,
	})
	if !ok {
		t.Fatal("expected BidiAppend capture decode")
	}
	if len(decoded.ToolEvents) != 1 {
		t.Fatalf("tool events = %d", len(decoded.ToolEvents))
	}
	if decoded.ToolEvents[0].Ordering != 4 {
		t.Fatalf("tool ordering = %d, want 4", decoded.ToolEvents[0].Ordering)
	}
}

func TestDecodeCaptureRequestSkipsMalformedHexRequestIDBeforePayload(t *testing.T) {
	payload := []byte("0a0663616c6c2d311209656469745f66696c651a117b227061746368223a2268656c6c6f227d")
	outer := appendProtoString(nil, 1, "deadbeef")
	outer = appendProtoVarint(outer, 2, 4)
	outer = appendProtoBytes(outer, 3, payload)

	decoded, ok := DecodeCaptureRequest(RequestCapture{
		Path: "/aiserver.v1.AiService/BidiAppend",
		Body: outer,
	})
	if !ok {
		t.Fatal("expected BidiAppend capture decode")
	}
	if decoded.Status != capture.DecodeStatusSuccess {
		t.Fatalf("status = %q error = %q", decoded.Status, decoded.Error)
	}
	if len(decoded.ToolEvents) != 1 {
		t.Fatalf("tool events = %d", len(decoded.ToolEvents))
	}
	if decoded.ToolEvents[0].CallID != "call-1" {
		t.Fatalf("tool call id = %q", decoded.ToolEvents[0].CallID)
	}
}

func cursorBidiAppendPayload(requestID string, seqno uint64, payload []byte) []byte {
	var out []byte
	out = appendProtoString(out, 1, requestID)
	out = appendProtoVarint(out, 2, seqno)
	out = appendProtoBytes(out, 3, payload)
	return out
}

func appendProtoString(dst []byte, fieldNumber uint64, value string) []byte {
	dst = appendProtoKey(dst, fieldNumber, 2)
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendProtoBytes(dst []byte, fieldNumber uint64, value []byte) []byte {
	dst = appendProtoKey(dst, fieldNumber, 2)
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendProtoVarint(dst []byte, fieldNumber uint64, value uint64) []byte {
	dst = appendProtoKey(dst, fieldNumber, 0)
	return binary.AppendUvarint(dst, value)
}

func appendProtoKey(dst []byte, fieldNumber uint64, wireType uint64) []byte {
	return binary.AppendUvarint(dst, fieldNumber<<3|wireType)
}
