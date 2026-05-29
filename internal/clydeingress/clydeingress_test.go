package clydeingress

import (
	"net/http"
	"testing"

	"goodkind.io/gklog/correlation"
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
