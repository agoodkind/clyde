package mitmcontrib

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"goodkind.io/clyde/internal/mitm"
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
