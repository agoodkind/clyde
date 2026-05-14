package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"goodkind.io/clyde/internal/livetrack"
)

// TestRPCRegistryRegisterReleaseRoundtrip verifies that a single
// Register call followed by a Release leaves the registry empty.
// This mirrors the pattern in each streaming handler: Register on
// entry, defer Release on exit.
func TestRPCRegistryRegisterReleaseRoundtrip(t *testing.T) {
	reg := newRPCRegistry()

	ctx := context.Background()
	sess, err := reg.Register(ctx, "daemon.rpc.test", RPCMeta{
		Method:     "/test/Method",
		Direction:  "inbound",
		PeerActor:  "127.0.0.1:9999",
		RequestID:  "",
		TraceID:    "",
		LeaseToken: "",
	}, rpcCloser{cancel: func() {}})
	if err != nil {
		t.Fatalf("Register returned unexpected error: %v", err)
	}
	if reg.Count() != 1 {
		t.Fatalf("count after Register = %d, want 1", reg.Count())
	}

	reg.Release(ctx, sess, "test.done")
	if reg.Count() != 0 {
		t.Fatalf("count after Release = %d, want 0", reg.Count())
	}
}

// TestRPCRegistryDrainRejectsNewRegister verifies that after Drain is
// called, subsequent Register calls return ErrRegistryClosed so
// streaming handlers correctly reject new connections during reload.
func TestRPCRegistryDrainRejectsNewRegister(t *testing.T) {
	reg := newRPCRegistry()
	ctx := context.Background()

	drainCtx, drainCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer drainCancel()

	// Drain with no sessions: should complete immediately.
	result := reg.Drain(drainCtx, "test.drain")
	if result.Final != livetrack.StateClosed {
		t.Fatalf("drain final state = %v, want StateClosed", result.Final)
	}

	// Any subsequent Register must be rejected.
	_, regErr := reg.Register(ctx, "daemon.rpc.test", RPCMeta{
		Method:     "/test/Method",
		Direction:  "inbound",
		PeerActor:  "",
		RequestID:  "",
		TraceID:    "",
		LeaseToken: "",
	}, rpcCloser{cancel: func() {}})
	if !errors.Is(regErr, livetrack.ErrRegistryClosed) {
		t.Fatalf("Register after Drain returned %v, want ErrRegistryClosed", regErr)
	}
}

// TestRPCRegistryForceCloseOnDeadlineCancelsContext verifies that
// when Drain hits its deadline with a live session, the force-close
// path invokes the rpcCloser which cancels the supplied context. This
// simulates the daemon reload path timing out an active gRPC stream.
func TestRPCRegistryForceCloseOnDeadlineCancelsContext(t *testing.T) {
	reg := livetrack.New[RPCMeta](livetrack.Options[RPCMeta]{
		Component:   "test.rpc",
		Concern:     "test.rpc",
		PollEvery:   10 * time.Millisecond,
		CloserGrace: 500 * time.Millisecond,
	})

	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()

	_, err := reg.Register(context.Background(), "daemon.rpc.test", RPCMeta{
		Method:     "/test/ForceClose",
		Direction:  "inbound",
		PeerActor:  "",
		RequestID:  "",
		TraceID:    "",
		LeaseToken: "",
	}, rpcCloser{cancel: streamCancel})
	if err != nil {
		t.Fatalf("Register returned unexpected error: %v", err)
	}

	// Drain with a short deadline so it hits the force-close path.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer drainCancel()
	result := reg.Drain(drainCtx, "test.force_close")

	if result.ForceClosed != 1 {
		t.Fatalf("force_closed = %d, want 1", result.ForceClosed)
	}

	// The stream context should be canceled because rpcCloser.Close
	// called streamCancel.
	select {
	case <-streamCtx.Done():
		// expected: context was canceled by force-close
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stream context was not canceled after force-close deadline")
	}
}

// TestRPCCloserCloseIsIdempotent verifies that calling rpcCloser.Close
// multiple times does not panic. The closer is backed by a
// context.CancelFunc which itself is idempotent.
func TestRPCCloserCloseIsIdempotent(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	closer := rpcCloser{cancel: cancel}

	if err := closer.Close("first"); err != nil {
		t.Fatalf("first Close returned error: %v", err)
	}
	if err := closer.Close("second"); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}

// TestRPCCloserNilCancel verifies that a zero-value rpcCloser (nil
// cancel func) does not panic when closed.
func TestRPCCloserNilCancel(t *testing.T) {
	closer := rpcCloser{cancel: nil}
	if err := closer.Close("reason"); err != nil {
		t.Fatalf("Close with nil cancel returned error: %v", err)
	}
}

// TestRPCMetaIsLivetrackMeta verifies the marker method satisfies the
// livetrack.Meta constraint at the type level. If this compiles, the
// constraint is satisfied; no runtime assertion is needed.
func TestRPCMetaIsLivetrackMeta(t *testing.T) {
	var _ livetrack.Meta = RPCMeta{}
}
