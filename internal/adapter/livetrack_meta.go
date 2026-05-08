package adapter

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/slogger"
)

// IngressMeta is the per-request metadata carried by every session
// registered in the adapter ingress registry. RouteFamily is the
// adapter route family ("openai" or "anthropic"). RequestID is the
// Clyde-assigned request identifier. CursorRequestID is the optional
// X-Cursor-Request-Id header value. Method and Path identify the HTTP
// entry point. Stream is true when the request negotiated a streaming
// response.
type IngressMeta struct {
	RouteFamily     string
	RequestID       string
	CursorRequestID string
	Method          string
	Path            string
	Stream          bool
}

// IsLivetrackMeta satisfies the livetrack.Meta marker constraint.
func (IngressMeta) IsLivetrackMeta() {}

// ingressConnCloser wraps a [net.Conn] so the registry can force-close
// the underlying TCP connection when a request is still in flight at
// drain deadline. Implementing [livetrack.Closer] with a real Close
// call ensures force-close terminates wedged SSE streams, tool-call
// bridges, and synthetic-content marker round-trips.
type ingressConnCloser struct {
	conn interface{ Close() error }
}

// Close satisfies [livetrack.Closer]. The reason is logged by the
// registry itself. Errors closing the underlying conn are also logged
// here so force-close failures are visible in the ingress concern log.
func (c ingressConnCloser) Close(reason string) error {
	if c.conn == nil {
		return nil
	}
	if err := c.conn.Close(); err != nil {
		slog.WarnContext(context.Background(), "adapter.ingress.conn_close_failed",
			"component", "adapter",
			"concern", slogger.ConcernAdapterHTTPIngress,
			"reason", reason,
			"err", err,
		)
		return fmt.Errorf("ingress conn close: %w", err)
	}
	return nil
}

// noopIngressCloser is used for handlers called via
// [httptest.NewRecorder] where no underlying [net.Conn] is available
// and force-close is a no-op.
type noopIngressCloser struct{}

// Close satisfies [livetrack.Closer].
func (noopIngressCloser) Close(_ string) error { return nil }

// newAdapterIngressRegistry constructs the per-Server [livetrack.Registry]
// for ingress request sessions. Component and Concern match the
// adapter's slog routing so ingress lifecycle events land in the
// correct concern log file.
func newAdapterIngressRegistry(log *slog.Logger) *livetrack.Registry[IngressMeta] {
	return livetrack.New[IngressMeta](livetrack.Options[IngressMeta]{
		Component:     "adapter",
		Concern:       slogger.ConcernAdapterHTTPIngress,
		Log:           log,
		PollEvery:     0,
		CloserGrace:   0,
		ParallelClose: false,
		Now:           nil,
	})
}
