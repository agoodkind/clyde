package daemon

import (
	"context"

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

// rpcCloser cancels the stream context when the registry force-closes a
// tracked RPC session. The cancel func is the one returned by
// [context.WithCancel] wrapping the stream's context; calling it causes
// the stream handler's ctx.Done() to fire, which terminates the
// blocking select loop in the handler and lets the handler return.
type rpcCloser struct {
	cancel context.CancelFunc
}

// Close cancels the stream context, which terminates the blocking
// send loop in the streaming RPC handler. The reason argument is
// provided by the registry caller but is not surfaced here because
// the handler's error return already carries context-canceled status.
func (c rpcCloser) Close(_ string) error {
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
