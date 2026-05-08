package daemon

import (
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/slogger"
)

// RPCMeta is the per-stream metadata carried by every session
// registered in the daemon RPC registry. Method is the full gRPC
// method name (e.g. "/clyde.v1.ClydeService/SubscribeRegistry").
// Direction is always "inbound" for server-side streams. PeerActor
// identifies the client address. RequestID and TraceID carry
// correlation context from the stream.
type RPCMeta struct {
	Method    string
	Direction string
	PeerActor string
	RequestID string
	TraceID   string
}

// IsLivetrackMeta satisfies the [livetrack.Meta] marker constraint.
func (RPCMeta) IsLivetrackMeta() {}

// rpcCloser cancels a stream context when force-closed. Calling
// cancel terminates the stream's server goroutine so reload drain
// does not wait for the client to disconnect.
type rpcCloser struct {
	cancel func()
}

// Close satisfies [livetrack.Closer]. The reason is logged by the
// registry itself; this closer only needs to cancel the context.
func (c rpcCloser) Close(_ string) error {
	if c.cancel == nil {
		return fmt.Errorf("rpc closer: cancel is nil")
	}
	c.cancel()
	return nil
}

// newRPCRegistry constructs the per-Server [livetrack.Registry] for
// inbound streaming gRPC sessions. Component and Concern match the
// daemon's slog routing so RPC lifecycle events land in the correct
// concern log file.
func newRPCRegistry() *livetrack.Registry[RPCMeta] {
	return livetrack.New[RPCMeta](livetrack.Options[RPCMeta]{
		Component:     "daemon",
		Concern:       slogger.ConcernDaemonRPCStreams,
		Log:           slog.Default(),
		PollEvery:     0,
		CloserGrace:   0,
		ParallelClose: false,
		Now:           nil,
	})
}
