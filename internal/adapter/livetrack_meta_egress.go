package adapter

import (
	"context"

	"goodkind.io/clyde/internal/livetrack"
)

// EgressMeta is the per-session metadata tracked for outbound provider
// calls. Each Provider HTTP call (Anthropic Messages, Codex Responses
// websocket), SSE reader goroutine, and retry attempt registers one
// session with this meta so the daemon reload drain can force-close
// wedged upstream connections under a bounded deadline.
//
// Component: adapter.egress
// Concern: adapter.providers
type EgressMeta struct {
	// Provider identifies the outbound provider (e.g. "anthropic", "codex").
	Provider string
	// UpstreamURL is the full URL the adapter is talking to. Empty for
	// websocket-only sessions that use the session-cache path.
	UpstreamURL string
	// UpstreamRequestID is the request-id the upstream returned, when
	// available. Populated after the upstream responds; empty during the
	// dialing and warmup phases.
	UpstreamRequestID string
	// AttemptNo is one-based. The parent egress session uses 0; each
	// retry attempt registers a nested child session with AttemptNo >= 1.
	AttemptNo int
	// ParentRequestID is the adapter-level request id that generated
	// this egress session. Allows operators to join egress sessions
	// back to the originating ingress request in log queries.
	ParentRequestID string
}

// IsLivetrackMeta satisfies [livetrack.Meta] so EgressMeta can
// parameterize [livetrack.Registry].
func (EgressMeta) IsLivetrackMeta() {}

// egressSessionKindHTTP identifies a provider HTTP call session.
const egressSessionKindHTTP = "adapter.egress.http"

// egressSessionKindWebsocket identifies a codex websocket transport session.
const egressSessionKindWebsocket = "adapter.egress.websocket"

// egressSessionKindAttempt identifies a retry attempt nested under a
// parent egress session.
const egressSessionKindAttempt = "adapter.egress.attempt"

// newEgressRegistry constructs the shared adapter egress registry, attached to
// the daemon lifecycle group as a quiet-relevant PhaseEgress member so every
// in-flight outbound provider call participates in the reload deadline and
// holds quiet until it completes.
func newEgressRegistry(group *livetrack.Group) *livetrack.Registry[EgressMeta] {
	return livetrack.Attach[EgressMeta](group, livetrack.MemberSpec{
		Phase:         livetrack.PhaseEgress,
		QuietRelevant: true,
		CancelNoWait:  false,
	}, livetrack.Options[EgressMeta]{
		Component:     "adapter.egress",
		Concern:       "adapter.providers",
		Log:           nil,
		PollEvery:     0,
		CloserGrace:   0,
		ParallelClose: true,
		Now:           nil,
	})
}

// contextCancelCloser implements [livetrack.Closer] by canceling a
// context. Used as the Closer for egress sessions whose force-close
// semantics are "cancel the request so Go's [http.Client] aborts the
// in-flight provider call."
type contextCancelCloser struct {
	cancel context.CancelFunc
}

func (c *contextCancelCloser) Close(reason string) error {
	c.cancel()
	return nil
}

// registerEgress registers an outbound provider call in reg and
// returns the session and a derived child context whose cancel is
// wired to the session's force-close path. The caller must defer
// reg.Release(...) after the upstream call returns.
//
// The returned context replaces the caller's ctx for all downstream
// I/O ([http.Client] Do, websocket dial, SSE scan) so that force-close
// under drain deadline cancels the request context and unblocks any
// blocked reader. When reg is nil (test builds without a registry) the
// original ctx is returned unchanged with a no-op release function.
func registerEgress(
	ctx context.Context,
	reg *livetrack.Registry[EgressMeta],
	kind string,
	meta EgressMeta,
	opts ...livetrack.RegisterOption,
) (context.Context, *livetrack.Session[EgressMeta], func(string)) {
	if reg == nil {
		return ctx, nil, func(string) {}
	}
	childCtx, cancel := context.WithCancel(ctx)
	sess, err := reg.Register(childCtx, kind, meta, &contextCancelCloser{cancel: cancel}, opts...)
	if err != nil {
		// Registry is draining: cancel immediately so the caller
		// returns ErrRegistryClosed-equivalent via context.
		cancel()
		return childCtx, nil, func(string) {}
	}
	return childCtx, sess, func(reason string) {
		reg.Release(childCtx, sess, reason)
		cancel()
	}
}
