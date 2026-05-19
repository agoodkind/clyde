package daemon

import (
	"context"
	"sync/atomic"
	"testing"
)

// TestRpcCloserFirstCallIsCooperative pins the CLYDE-437 contract that
// the first invocation of the registered Closer raises the draining
// flag but does NOT cancel the stream context. The streaming handler
// observes the flag between events and returns naturally, so the
// gRPC client sees clean stream completion instead of codes.Canceled.
func TestRpcCloserFirstCallIsCooperative(t *testing.T) {
	cancelCalls := atomic.Int32{}
	draining := &atomic.Bool{}
	cancel := func() {
		cancelCalls.Add(1)
	}
	closer := rpcCloser{cancel: cancel, draining: draining}

	if err := closer.Close("grpc.reload"); err != nil {
		t.Fatalf("first Close returned %v, want nil", err)
	}
	if !draining.Load() {
		t.Fatalf("first Close should raise draining flag")
	}
	if got := cancelCalls.Load(); got != 0 {
		t.Fatalf("first Close called cancel %d times, want 0 (cooperative)", got)
	}
}

// TestRpcCloserSecondCallCancelsAsSafetyNet pins the cap-elapsed
// safety-net contract: when the handler has not observed the
// draining flag in time (handler wedged on a slow Send, or
// blockingly waiting on a downstream), the second invocation
// cancels the stream context outright so the registry can complete
// its drain.
func TestRpcCloserSecondCallCancelsAsSafetyNet(t *testing.T) {
	cancelCalls := atomic.Int32{}
	draining := &atomic.Bool{}
	cancel := func() {
		cancelCalls.Add(1)
	}
	closer := rpcCloser{cancel: cancel, draining: draining}

	_ = closer.Close("grpc.reload")
	_ = closer.Close("grpc.reload.cap_elapsed")

	if got := cancelCalls.Load(); got != 1 {
		t.Fatalf("second Close called cancel %d times, want 1 (safety net)", got)
	}
	if !draining.Load() {
		t.Fatalf("draining flag should remain set after safety-net call")
	}
}

// TestRpcCloserHandlerLoopReturnsOnDrainSignal models the streaming
// RPC handler's send loop and asserts that when the registered
// Closer raises the draining flag, the handler exits its select
// loop on the next iteration. The simulated handler returns nil
// (clean completion) rather than context.Canceled. Uses a buffered
// wake channel sized so neither the sender nor the handler can
// deadlock regardless of scheduling order.
func TestRpcCloserHandlerLoopReturnsOnDrainSignal(t *testing.T) {
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	draining := &atomic.Bool{}
	wake := make(chan struct{}, 4)

	closer := rpcCloser{cancel: streamCancel, draining: draining}

	handlerExit := make(chan error, 1)
	go func() {
		for {
			if draining.Load() {
				handlerExit <- nil
				return
			}
			select {
			case <-wake:
				continue
			case <-streamCtx.Done():
				handlerExit <- streamCtx.Err()
				return
			}
		}
	}()

	// Signal drain. The handler may already be parked in select; the
	// wake below unblocks it so it observes the flag on the next
	// loop turn. Either ordering produces clean completion.
	_ = closer.Close("grpc.reload")
	wake <- struct{}{}

	select {
	case err := <-handlerExit:
		if err != nil {
			t.Fatalf("handler exited with err=%v, want nil (clean completion)", err)
		}
	case <-streamCtx.Done():
		t.Fatalf("stream context cancelled before handler observed draining flag")
	}
}
