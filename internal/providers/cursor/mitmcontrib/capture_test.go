package mitmcontrib

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"testing"
	"time"

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
		t.Fatalf("provider id = %q want %q", claim.ProviderID, providerID)
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

func TestBuildCaptureExtensionAddsIdentityTraceAndBidiDiagnostic(t *testing.T) {
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

	extension := routeProvider{}.BuildCaptureExtension(mitm.CaptureExchange{
		CapturedAt:          time.Unix(123, 456).UTC(),
		RequestHeader:       headers,
		RequestBody:         body,
		DecodedRequestBody:  body,
		ResponseHeader:      http.Header{"Content-Type": []string{"application/json"}},
		ResponseStatus:      http.StatusAccepted,
		RequestBytes:        int64(len(body)),
		ResponseBytes:       12,
		Method:              http.MethodPost,
		Path:                "/aiserver.v1.AiService/BidiAppend",
		Host:                "api2.cursor.sh",
		Concern:             "cursor.bidi",
		RequestRawPath:      "/tmp/request.raw",
		ResponseRawPath:     "/tmp/response.raw",
		RequestContentType:  "application/protobuf",
		ResponseContentType: "application/json",
		HookName:            "",
	})
	if extension == nil {
		t.Fatal("expected capture extension")
	}
	raw, err := extension.MarshalJSONLine()
	if err != nil {
		t.Fatalf("encode capture extension: %v", err)
	}
	if bytes.Contains(raw, []byte(sentinel)) {
		t.Fatalf("diagnostic JSON leaked sentinel: %s", raw)
	}
	var event CaptureExtension
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("unmarshal capture extension: %v", err)
	}
	if event.ConcernName != "cursor.bidi" {
		t.Fatalf("concern = %q want cursor.bidi", event.ConcernName)
	}
	if event.TraceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("trace id = %q", event.TraceID)
	}
	if event.Diagnostic == nil {
		t.Fatal("expected BidiAppend diagnostic")
	}
	if event.Diagnostic.RequestID != requestID {
		t.Fatalf("diagnostic request id = %q", event.Diagnostic.RequestID)
	}
	if event.Diagnostic.AppendSeqno != 42 {
		t.Fatalf("diagnostic append seqno = %d", event.Diagnostic.AppendSeqno)
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
