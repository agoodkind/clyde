package mcpserver

import (
	"context"
	"testing"

	"goodkind.io/gklog/correlation"
)

// TestNewToolCallContextMintsFreshTracePerCall proves each MCP tool call starts
// its own trace and span instead of inheriting (and reusing) the one shared
// correlation seeded on the serve process's base context. Two calls over the
// same base context must produce distinct trace and span ids, neither equal to
// the inherited one.
func TestNewToolCallContextMintsFreshTracePerCall(t *testing.T) {
	t.Parallel()

	// The serve process seeds one correlation on the base context; both tool
	// calls below derive from it, as mcp-go derives every handler context from
	// the one base context passed to the stdio server.
	base := correlation.WithContext(context.Background(), correlation.New("serve-process"))
	inherited := correlation.FromContext(base)

	_, first := newToolCallContext(base, "req-1")
	_, second := newToolCallContext(base, "req-2")

	if !first.Valid() || !second.Valid() {
		t.Fatalf("expected valid correlations, got %#v and %#v", first, second)
	}
	if first.TraceID == second.TraceID {
		t.Fatalf("two tool calls share trace id %q, want a fresh trace per call", first.TraceID)
	}
	if first.SpanID == second.SpanID {
		t.Fatalf("two tool calls share span id %q, want a fresh span per call", first.SpanID)
	}
	if first.TraceID == inherited.TraceID {
		t.Fatalf("tool call reused the inherited serve trace %q, want a fresh one", inherited.TraceID)
	}
	if first.RequestID != "req-1" || second.RequestID != "req-2" {
		t.Fatalf("request ids = %q and %q, want req-1 and req-2", first.RequestID, second.RequestID)
	}
}
