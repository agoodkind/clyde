package clydeingress

import (
	"context"
	"net/http"
	"testing"

	"goodkind.io/gklog/correlation"
	"google.golang.org/grpc/metadata"
)

func TestFromHTTPHeaderReadsClydeHeaders(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set(HeaderTraceID, "0123456789abcdef0123456789abcdef")
	header.Set(HeaderSpanID, "0123456789abcdef")
	header.Set(HeaderParentSpanID, "fedcba9876543210")
	header.Set(HeaderRequestID, "req-1")
	header.Set(HeaderUpstreamRequestID, "upstream-req")
	header.Set(HeaderUpstreamResponseID, "upstream-resp")

	corr := FromHTTPHeader(header, "req-fallback")

	if corr.RequestID != "req-1" {
		t.Fatalf("request id = %q, want req-1", corr.RequestID)
	}
	if corr.TraceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("trace id = %q", corr.TraceID)
	}
	// gklog's FromHTTPHeader resolves x-parent-span-id last, so the
	// explicit parent header wins over the implicit parent derived
	// from x-span-id.
	if corr.ParentSpanID != "fedcba9876543210" {
		t.Fatalf("parent span = %q, want x-parent-span-id value", corr.ParentSpanID)
	}
	if corr.SpanID == "" || corr.SpanID == "0123456789abcdef" {
		t.Fatalf("span id should be a fresh child, got %q", corr.SpanID)
	}
	if got := UpstreamRequestID(corr); got != "upstream-req" {
		t.Fatalf("upstream request id = %q", got)
	}
	if got := UpstreamResponseID(corr); got != "upstream-resp" {
		t.Fatalf("upstream response id = %q", got)
	}
}

func TestFromHTTPHeaderClaudeSessionResolvesChatKey(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set(HeaderClaudeCodeSessionID, "claude-sess")
	corr := FromHTTPHeader(header, "req-2")

	if got := ChatKey(corr); got != "claude-sess" {
		t.Fatalf("chat key = %q", got)
	}
	if got := ChatKeySource(corr); got != "native" {
		t.Fatalf("chat key source = %q, want native", got)
	}
	if got := ChatRootKey(corr); got != "claude-sess" {
		t.Fatalf("chat root key = %q", got)
	}
}

func TestSetHTTPHeadersWritesClydeFormat(t *testing.T) {
	t.Parallel()

	corr := correlation.New("req-3")
	corr = WithUpstreamRequestID(corr, "u-req")
	corr = WithUpstreamResponseID(corr, "u-resp")

	header := http.Header{}
	SetHTTPHeaders(corr, header)
	if header.Get(HeaderTraceID) == "" {
		t.Fatalf("trace id header should be set")
	}
	if header.Get(HeaderSpanID) == "" {
		t.Fatalf("span id header should be set")
	}
	if header.Get(HeaderRequestID) != "req-3" {
		t.Fatalf("request id header = %q", header.Get(HeaderRequestID))
	}
	if header.Get(HeaderTraceparent) == "" {
		t.Fatalf("traceparent header should be set")
	}
	if header.Get(HeaderUpstreamRequestID) != "" {
		t.Fatalf("SetHTTPHeaders should not write upstream headers; HTTPHeaders does")
	}
}

func TestHTTPHeadersIncludesUpstreamFields(t *testing.T) {
	t.Parallel()

	corr := correlation.New("req-4")
	corr = WithUpstreamRequestID(corr, "u-req")
	corr = WithUpstreamResponseID(corr, "u-resp")

	header := HTTPHeaders(corr)
	if header.Get(HeaderUpstreamRequestID) != "u-req" {
		t.Fatalf("upstream request header = %q", header.Get(HeaderUpstreamRequestID))
	}
	if header.Get(HeaderUpstreamResponseID) != "u-resp" {
		t.Fatalf("upstream response header = %q", header.Get(HeaderUpstreamResponseID))
	}
}

func TestWithChatKeyDoesNotOverwriteExistingValue(t *testing.T) {
	t.Parallel()

	corr := correlation.New("req-5")
	corr = WithChatKey(corr, "first")
	corr = WithChatKey(corr, "second")
	corr = WithChatKey(corr, "   ")

	if got := ChatKey(corr); got != "first" {
		t.Fatalf("chat key = %q, want first", got)
	}
}

func TestWithChatIdentityRewritesAllFields(t *testing.T) {
	t.Parallel()

	corr := correlation.New("req-6")
	corr = WithChatIdentity(corr, "root.b01", "derived", "root", "b01")

	if got := ChatKey(corr); got != "root.b01" {
		t.Fatalf("chat key = %q", got)
	}
	if got := ChatKeySource(corr); got != "derived" {
		t.Fatalf("chat key source = %q", got)
	}
	if got := ChatRootKey(corr); got != "root" {
		t.Fatalf("chat root key = %q", got)
	}
	if got := ChatBranchKey(corr); got != "b01" {
		t.Fatalf("chat branch key = %q", got)
	}

	corr = WithChatIdentity(corr, "", "", "", "")
	if got := ChatKey(corr); got != "" {
		t.Fatalf("chat key should clear, got %q", got)
	}
}

func TestUpstreamHelpersRoundTripThroughAttrs(t *testing.T) {
	t.Parallel()

	corr := correlation.New("req-7")
	corr = WithUpstreamRequestID(corr, "u-req")
	corr = WithUpstreamResponseID(corr, "u-resp")

	attrs := corr.Attrs()
	values := map[string]string{}
	for _, attr := range attrs {
		values[attr.Key] = attr.Value.String()
	}
	if values[AttrKeyUpstreamRequestID] != "u-req" {
		t.Fatalf("attr upstream_request_id = %q", values[AttrKeyUpstreamRequestID])
	}
	if values[AttrKeyUpstreamResponseID] != "u-resp" {
		t.Fatalf("attr upstream_response_id = %q", values[AttrKeyUpstreamResponseID])
	}
}

func TestFromIncomingMetadataAddsUpstreamFields(t *testing.T) {
	t.Parallel()

	md := metadata.MD{}
	md.Set("x-clyde-request-id", "req-md")
	md.Set("x-clyde-trace-id", "11111111111111111111111111111111")
	md.Set("x-clyde-span-id", "2222222222222222")
	md.Set("x-upstream-request-id", "u-md-req")
	md.Set("x-upstream-response-id", "u-md-resp")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	corr := FromIncomingMetadata(ctx)
	// gklog speaks the neutral x-request-id / x-trace-id keys on the
	// wire, so the legacy x-clyde-* metadata above contributes only the
	// upstream identifiers via this layer. The trace/span fields fall
	// through to gklog defaults (a fresh trace in the absence of neutral
	// keys), which is acceptable: daemon clients now write the neutral
	// keys via NewOutgoingContext.
	if got := UpstreamRequestID(corr); got != "u-md-req" {
		t.Fatalf("upstream request id from metadata = %q", got)
	}
	if got := UpstreamResponseID(corr); got != "u-md-resp" {
		t.Fatalf("upstream response id from metadata = %q", got)
	}
}

func TestNewOutgoingContextWritesUpstreamMetadata(t *testing.T) {
	t.Parallel()

	corr := correlation.New("req-out")
	corr = WithUpstreamRequestID(corr, "u-out-req")
	corr = WithUpstreamResponseID(corr, "u-out-resp")
	ctx := correlation.WithContext(context.Background(), corr)

	ctx = NewOutgoingContext(ctx)
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatalf("outgoing metadata missing")
	}
	if got := md.Get("x-upstream-request-id"); len(got) != 1 || got[0] != "u-out-req" {
		t.Fatalf("upstream request md = %v", got)
	}
	if got := md.Get("x-upstream-response-id"); len(got) != 1 || got[0] != "u-out-resp" {
		t.Fatalf("upstream response md = %v", got)
	}
	if got := md.Get("x-request-id"); len(got) != 1 || got[0] != "req-out" {
		t.Fatalf("standard request md = %v", got)
	}
}
