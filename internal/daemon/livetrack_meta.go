package daemon

import (
	"context"
	"sync/atomic"

	"goodkind.io/clyde/internal/livetrack"
)

// RPCMeta is the per-stream metadata stored alongside each
// livetrack.Session the daemon registers for an inbound streaming
// RPC handler. The fields let operators query "which gRPC stream is
// this and who owns it" from a registry snapshot without
// dragging the rest of the server state into every snapshot.
//
// IsLivetrackMeta is the empty marker method the livetrack package
// requires; its only purpose is to constrain the registry's type
// parameter at compile time.
type RPCMeta struct {
	// Method is the full gRPC method name, e.g. "/clyde.v1.ClydeService/TailTranscript".
	Method string
	// Direction is "inbound" for server-streaming RPCs the daemon serves to callers.
	Direction string
	// PeerActor is the network address of the connected gRPC peer.
	PeerActor string
	// RequestID is the correlation request ID extracted from incoming metadata.
	RequestID string
	// TraceID is the correlation trace ID extracted from incoming metadata.
	TraceID string
	// LeaseToken is the foreground-lease token when the stream is tied to a lease.
	// Empty when no lease is active for this stream.
	LeaseToken string
}

// IsLivetrackMeta is the empty marker method that satisfies the
// livetrack.Meta constraint. The body is intentionally empty.
func (RPCMeta) IsLivetrackMeta() {}

// rpcCloser is the livetrack Closer for a streaming RPC session. It
// participates in graceful drain in two stages:
//
//  1. The first invocation (registry signalling drain) raises the
//     draining [atomic.Bool]. The streaming RPC handler checks this
//     flag between events and returns naturally after the current
//     event has flushed, so the client sees the stream complete
//     cleanly instead of cancelled mid-frame.
//  2. The second invocation (cap-elapsed safety net) cancels the
//     stream context outright, terminating the handler's blocking
//     select with [codes.Canceled]. This is the bounded fallback
//     when the handler cannot otherwise observe the drain flag in
//     time.
//
// CLYDE-437: before this change the Closer cancelled context
// immediately on first call, recording force_closed:1 with
// duration_ms:0 in logs and killing in-flight LLM-bearing streams
// even though the gRPC reload drain waits ten minutes for natural
// completion. The cooperative shape lets the handler return clean
// while preserving the cap-elapsed safety net.
type rpcCloser struct {
	cancel   context.CancelFunc
	draining *atomic.Bool
}

// Close raises the cooperative draining flag on the first call and
// cancels the stream context on subsequent calls. The reason argument
// is provided by the registry caller but is not surfaced here because
// the handler's error return already carries the relevant status.
func (c rpcCloser) Close(_ string) error {
	if c.draining != nil && c.draining.CompareAndSwap(false, true) {
		return nil
	}
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

// newRPCRegistry constructs the per-daemon livetrack registry for
// inbound streaming RPC sessions. Component and Concern match the
// existing daemon.rpc concern so registry lifecycle events route to
// the daemon/rpc/streams concern log.
func newRPCRegistry() *livetrack.Registry[RPCMeta] {
	return livetrack.New[RPCMeta](livetrack.Options[RPCMeta]{
		Component:     "daemon.rpc",
		Concern:       "daemon.rpc",
		Log:           nil,
		PollEvery:     0,
		CloserGrace:   0,
		ParallelClose: false,
		Now:           nil,
	})
}
