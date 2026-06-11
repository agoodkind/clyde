package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"goodkind.io/clyde/internal/livetrack"
)

// TestToolCallMiddlewareRegistersAndReleases verifies that toolCallMiddleware
// registers a tool call before dispatching the inner handler and releases it
// after the handler returns.
func TestToolCallMiddlewareRegistersAndReleases(t *testing.T) {
	t.Parallel()
	reg := livetrack.Attach[MCPMeta](livetrack.NewGroup(livetrack.GroupOptions{Log: nil}), livetrack.MemberSpec{Phase: livetrack.PhaseIngress, QuietRelevant: true, CancelNoWait: false}, livetrack.Options[MCPMeta]{
		Component: "test",
		Concern:   "test.mcp.register",
		PollEvery: 5 * time.Millisecond,
	})

	var countDuringCall atomic.Int32
	inner := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		countDuringCall.Store(int32(reg.Count()))
		return mcp.NewToolResultText("ok"), nil
	}

	mw := toolCallMiddleware(reg, "test-server")
	wrapped := mw(inner)

	req := mcp.CallToolRequest{}
	req.Params.Name = "test_tool"
	result, err := wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("wrapped handler: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// During the call the registry entry must exist (count == 1).
	if got := countDuringCall.Load(); got != 1 {
		t.Fatalf("count during call: got %d, want 1", got)
	}

	// After return the registry entry must be released.
	if got := reg.Count(); got != 0 {
		t.Fatalf("count after call: got %d, want 0", got)
	}
}

// TestToolCallMiddlewareMetaFields verifies that MCPMeta is populated with the
// correct tool name, op, and server name from the request and closure.
func TestToolCallMiddlewareMetaFields(t *testing.T) {
	t.Parallel()
	reg := livetrack.Attach[MCPMeta](livetrack.NewGroup(livetrack.GroupOptions{Log: nil}), livetrack.MemberSpec{Phase: livetrack.PhaseIngress, QuietRelevant: true, CancelNoWait: false}, livetrack.Options[MCPMeta]{
		Component: "test",
		Concern:   "test.mcp.meta",
		PollEvery: 5 * time.Millisecond,
	})

	var capturedMeta MCPMeta
	inner := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		snap := reg.Snapshot()
		if len(snap) == 1 {
			capturedMeta = snap[0].Meta
		}
		return mcp.NewToolResultText("ok"), nil
	}

	mw := toolCallMiddleware(reg, "my-server")
	wrapped := mw(inner)

	req := mcp.CallToolRequest{}
	req.Params.Name = "clyde_list_conversations"
	stampCallToolRequestID("rpc-1", &req)
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped handler: %v", err)
	}

	if capturedMeta.ServerName != "my-server" {
		t.Errorf("ServerName: got %q, want %q", capturedMeta.ServerName, "my-server")
	}
	if capturedMeta.Tool != "clyde_list_conversations" {
		t.Errorf("Tool: got %q, want %q", capturedMeta.Tool, "clyde_list_conversations")
	}
	if capturedMeta.Op != "tool_call" {
		t.Errorf("Op: got %q, want %q", capturedMeta.Op, "tool_call")
	}
	if capturedMeta.RequestID != "rpc-1" {
		t.Errorf("RequestID: got %q, want %q", capturedMeta.RequestID, "rpc-1")
	}
}

func TestMCPHookStampsRequestID(t *testing.T) {
	t.Parallel()
	req := mcp.CallToolRequest{}
	hooks := newHooks()
	if len(hooks.OnBeforeCallTool) != 1 {
		t.Fatalf("before call tool hooks: got %d, want 1", len(hooks.OnBeforeCallTool))
	}
	hooks.OnBeforeCallTool[0](context.Background(), float64(42), &req)
	if got := req.Header.Get(mcpRequestIDHeader); got != "42" {
		t.Fatalf("request id header: got %q, want %q", got, "42")
	}
}

// TestToolCallMiddlewareRejectsWhenDraining verifies that toolCallMiddleware
// returns an error (and does not call the inner handler) when the registry has
// begun draining.
func TestToolCallMiddlewareRejectsWhenDraining(t *testing.T) {
	t.Parallel()
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	reg := livetrack.Attach[MCPMeta](group, livetrack.MemberSpec{Phase: livetrack.PhaseIngress, QuietRelevant: true, CancelNoWait: false}, livetrack.Options[MCPMeta]{
		Component: "test",
		Concern:   "test.mcp.reject",
		PollEvery: 5 * time.Millisecond,
	})

	// Drain immediately through the lifecycle group so the registry moves to
	// Draining and rejects new tool calls.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer drainCancel()
	go func() {
		group.Quiesce(drainCtx, "test.drain", livetrack.Budget{Cap: 10 * time.Millisecond, IdleGrace: 0})
	}()

	// Wait until the registry is no longer Open.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if reg.State() != livetrack.StateOpen {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	innerCalled := false
	inner := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		innerCalled = true
		return mcp.NewToolResultText("should not run"), nil
	}

	mw := toolCallMiddleware(reg, "test-server")
	wrapped := mw(inner)

	req := mcp.CallToolRequest{}
	req.Params.Name = "test_tool"
	_, err := wrapped(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when registry is draining, got nil")
	}
	if !errors.Is(err, livetrack.ErrRegistryClosed) {
		t.Fatalf("expected ErrRegistryClosed in error chain, got: %v", err)
	}
	if innerCalled {
		t.Fatal("inner handler should not have been called when registry is draining")
	}
}

// TestServeStdioLockedDrainsOnExit verifies that serveStdioLocked drains the
// registry when the server context is canceled. The test cancels the parent
// context to cause Listen to return, then asserts that the registry count
// reaches zero and the closer is invoked.
func TestServeStdioLockedDrainsOnExit(t *testing.T) {
	t.Parallel()
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	reg := livetrack.Attach[MCPMeta](group, livetrack.MemberSpec{
		Phase:         livetrack.PhaseIngress,
		QuietRelevant: true,
		CancelNoWait:  false,
	}, livetrack.Options[MCPMeta]{
		Component:   "test",
		Concern:     "test.mcp.drain",
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})

	// Register a long-lived tool call manually so there is something to drain.
	closer := &testReleaseCloser{released: nil}
	sess, err := reg.Register(context.Background(), "mcp.tool_call", MCPMeta{
		ServerName: "test",
		Tool:       "fake_tool",
		Op:         "tool_call",
	}, closer)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Build a minimal MCP server that does nothing.
	mcpSrv := server.NewMCPServer("test", "0.0.0")

	// Use a pipe so stdin blocks until we close the write end, which causes
	// the upstream stdio server to return EOF and exit Listen cleanly.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdinW.Close()
	})
	var stdout bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveStdioLocked(ctx, mcpSrv, group, 200*time.Millisecond, stdinR, &stdout)
	}()

	// Cancel the context to make serveStdioLocked exit, then close stdin so
	// the upstream listener unblocks.
	cancel()
	_ = stdinW.Close()

	// Wait for serveStdioLocked to finish.
	select {
	case serveErr := <-serveDone:
		// Nil or context.Canceled are both acceptable.
		if serveErr != nil && !errors.Is(serveErr, context.Canceled) && !errors.Is(serveErr, io.EOF) {
			t.Logf("serveStdioLocked returned: %v (non-fatal)", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveStdioLocked did not return within 5s")
	}

	// The drain call inside serveStdioLocked should have force-closed the
	// entry because it was still registered when Listen returned.
	if reg.Count() != 0 {
		t.Fatalf("registry count after drain: got %d, want 0", reg.Count())
	}
	// The closer must have been called exactly once.
	if closer.count.Load() != 1 {
		t.Fatalf("closer call count: got %d, want 1", closer.count.Load())
	}
	// The entry was already removed by force-close so Release is a no-op.
	reg.Release(context.Background(), sess, "test.cleanup")
}

// TestForceCloseOnDeadlineCancelsHandlerContext verifies that when Drain hits
// its deadline, the closer cancels the handler's derived context. The inner
// handler is a goroutine that blocks on the context; the test asserts it
// unblocks within a reasonable window after force-close fires.
func TestForceCloseOnDeadlineCancelsHandlerContext(t *testing.T) {
	t.Parallel()
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	reg := livetrack.Attach[MCPMeta](group, livetrack.MemberSpec{Phase: livetrack.PhaseIngress, QuietRelevant: true, CancelNoWait: false}, livetrack.Options[MCPMeta]{
		Component:   "test",
		Concern:     "test.mcp.forceclose",
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 500 * time.Millisecond,
	})

	// handlerExited signals when the (simulated) handler observes context
	// cancellation.
	handlerExited := make(chan struct{})

	// handlerCtx is canceled by the closer; the handler goroutine watches it.
	handlerCtx, handlerCancel := context.WithCancel(context.Background())
	closer := &contextCancelCloser{cancel: handlerCancel}

	sess, err := reg.Register(context.Background(), "mcp.tool_call", MCPMeta{
		ServerName: "test",
		Tool:       "blocking_tool",
		Op:         "tool_call",
	}, closer)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Simulate a handler that blocks until its context is canceled.
	go func() {
		<-handlerCtx.Done()
		close(handlerExited)
		reg.Release(context.Background(), sess, "test.handler.done")
	}()

	// Drain through the group with a very short cap to trigger force-close
	// quickly. The force-close cancels the handler context; the registry's own
	// force-close count is unit-tested in livetrack/drain_test.go.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer drainCancel()
	group.Quiesce(drainCtx, "test.forceclose", livetrack.Budget{Cap: 10 * time.Millisecond, IdleGrace: 0})

	// The handler goroutine should have observed the cancellation.
	select {
	case <-handlerExited:
		// expected
	case <-time.After(time.Second):
		t.Fatal("handler did not observe context cancellation within 1s after force-close")
	}
}

// testReleaseCloser records how many times Close was called and signals
// the released channel on first Close. Used by drain tests to verify
// the force-close fan-out reached the registered call.
type testReleaseCloser struct {
	count    atomic.Int32
	released chan struct{}
}

// Close increments the call counter and closes the released channel on
// first invocation. It satisfies livetrack.Closer.
func (c *testReleaseCloser) Close(_ string) error {
	if c.count.Add(1) == 1 && c.released != nil {
		close(c.released)
	}
	return nil
}
