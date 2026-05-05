package correlation

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestNewCreatesValidTraceAndSpan(t *testing.T) {
	t.Parallel()

	corr := New("req-1")
	if !corr.Valid() {
		t.Fatalf("correlation should be valid: %#v", corr)
	}
	if corr.RequestID != "req-1" {
		t.Fatalf("request id = %q, want req-1", corr.RequestID)
	}
	if corr.Traceparent() == "" {
		t.Fatalf("traceparent should be populated")
	}
}

func TestFromHTTPHeaderUsesTraceparentAsParent(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set(HeaderTraceparent, "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	corr := FromHTTPHeader(header, "req-2")

	if corr.TraceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("trace id = %q", corr.TraceID)
	}
	if corr.ParentSpanID != "0123456789abcdef" {
		t.Fatalf("parent span id = %q", corr.ParentSpanID)
	}
	if corr.SpanID == "" || corr.SpanID == corr.ParentSpanID {
		t.Fatalf("span id should be a new child span: %#v", corr)
	}
}

func TestAttrsIncludesCorrelationFields(t *testing.T) {
	t.Parallel()

	corr := Context{
		TraceID:              "0123456789abcdef0123456789abcdef",
		SpanID:               "0123456789abcdef",
		ParentSpanID:         "fedcba9876543210",
		RequestID:            "req-3",
		CursorRequestID:      "cursor-req",
		CursorConversationID: "cursor-conv",
		CursorGenerationID:   "cursor-gen",
		UpstreamRequestID:    "upstream-req",
		UpstreamResponseID:   "upstream-resp",
	}
	got := attrMap(corr.Attrs())
	for _, key := range []string{
		"trace_id",
		"span_id",
		"parent_span_id",
		"request_id",
		"cursor_request_id",
		"cursor_conversation_id",
		"cursor_generation_id",
		"upstream_request_id",
		"upstream_response_id",
	} {
		if got[key] == "" {
			t.Fatalf("missing attr %q in %#v", key, got)
		}
	}
}

func TestAppendAttrsSkipsExistingKeys(t *testing.T) {
	t.Parallel()

	corr := Context{
		TraceID:   "11111111111111111111111111111111",
		SpanID:    "2222222222222222",
		RequestID: "corr-request",
	}
	got := attrMap(AppendAttrs([]slog.Attr{slog.String("request_id", "explicit-request")}, corr))
	if got["request_id"] != "explicit-request" {
		t.Fatalf("request_id = %q, want explicit-request", got["request_id"])
	}
	if got["trace_id"] != string(corr.TraceID) {
		t.Fatalf("trace_id = %q, want %q", got["trace_id"], corr.TraceID)
	}
	if got["span_id"] != string(corr.SpanID) {
		t.Fatalf("span_id = %q, want %q", got["span_id"], corr.SpanID)
	}
}

func TestMetadataRoundTripCreatesChildSpan(t *testing.T) {
	t.Parallel()

	parent := Context{
		TraceID:   "0123456789abcdef0123456789abcdef",
		SpanID:    "0123456789abcdef",
		RequestID: "req-4",
	}
	ctx := metadata.NewIncomingContext(context.Background(), parent.Metadata())
	child := FromIncomingMetadata(ctx)

	if child.TraceID != parent.TraceID {
		t.Fatalf("trace id = %q, want %q", child.TraceID, parent.TraceID)
	}
	if child.ParentSpanID != parent.SpanID {
		t.Fatalf("parent span id = %q, want %q", child.ParentSpanID, parent.SpanID)
	}
	if child.SpanID == "" || child.SpanID == parent.SpanID {
		t.Fatalf("child span should differ from parent: %#v", child)
	}
}

func attrMap(attrs []slog.Attr) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[attr.Key] = attr.Value.String()
	}
	return out
}

func TestChatKeyAttrRoundTrip(t *testing.T) {
	t.Parallel()

	c := New("req-ck")
	if c.ChatKey != "" {
		t.Fatalf("ChatKey should start empty, got %q", c.ChatKey)
	}
	c2 := c.WithChatKey("conv-abc")
	if c2.ChatKey != "conv-abc" {
		t.Fatalf("WithChatKey did not set value: %q", c2.ChatKey)
	}
	c3 := c2.WithChatKey("conv-xyz")
	if c3.ChatKey != "conv-abc" {
		t.Fatalf("WithChatKey overwrote existing value: %q", c3.ChatKey)
	}
	c4 := c2.WithChatKey("   ")
	if c4.ChatKey != "conv-abc" {
		t.Fatalf("WithChatKey corrupted value with whitespace: %q", c4.ChatKey)
	}
	got := attrMap(c2.Attrs())
	if got["chat_key"] != "conv-abc" {
		t.Fatalf("Attrs() did not carry chat_key: %v", got)
	}
}

func TestFromHTTPHeaderResolvesChatKeyHeaderPrecedence(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set(HeaderCursorConversationID, "cursor-conv-1")
	header.Set(HeaderClaudeCodeSessionID, "claude-sess-1")
	c := FromHTTPHeader(header, "req-1")
	if c.ChatKey != "cursor-conv-1" {
		t.Fatalf("cursor conversation id should win, got %q", c.ChatKey)
	}

	header2 := http.Header{}
	header2.Set(HeaderClaudeCodeSessionID, "claude-sess-2")
	c2 := FromHTTPHeader(header2, "req-2")
	if c2.ChatKey != "claude-sess-2" {
		t.Fatalf("claude code session id should resolve when cursor missing, got %q", c2.ChatKey)
	}

	header3 := http.Header{}
	c3 := FromHTTPHeader(header3, "req-3")
	if c3.ChatKey != "" {
		t.Fatalf("ChatKey should be empty when neither header set, got %q", c3.ChatKey)
	}
}
