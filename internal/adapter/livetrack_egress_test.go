package adapter

import (
	"context"
	"testing"
	"time"

	"goodkind.io/clyde/internal/livetrack"
)

// fakeCloser records whether Close was called and with what reason.
type fakeCloser struct {
	closed bool
	reason string
}

func (f *fakeCloser) Close(reason string) error {
	f.closed = true
	f.reason = reason
	return nil
}

func TestRegisterEgressRegistersAndReleasesSession(t *testing.T) {
	t.Parallel()
	reg := newEgressRegistry()
	ctx := context.Background()

	egressCtx, sess, release := registerEgress(ctx, reg, egressSessionKindHTTP, EgressMeta{
		Provider:          "anthropic",
		UpstreamURL:       "https://api.anthropic.com/v1/messages",
		UpstreamRequestID: "",
		AttemptNo:         0,
		ParentRequestID:   "req-abc",
	})
	if egressCtx == nil {
		t.Fatal("registerEgress returned nil context")
	}
	if sess == nil {
		t.Fatal("registerEgress returned nil session")
	}
	if reg.Count() != 1 {
		t.Fatalf("expected 1 session, got %d", reg.Count())
	}

	release("done")

	// After release the context cancel should have fired (count back to 0).
	if reg.Count() != 0 {
		t.Fatalf("expected 0 sessions after release, got %d", reg.Count())
	}
	if egressCtx.Err() == nil {
		t.Error("context should be canceled after release")
	}
}

func TestRegisterEgressNilRegistryIsNoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	egressCtx, sess, release := registerEgress(ctx, nil, egressSessionKindHTTP, EgressMeta{
		Provider:          "anthropic",
		UpstreamURL:       "https://api.anthropic.com/v1/messages",
		UpstreamRequestID: "",
		AttemptNo:         0,
		ParentRequestID:   "req-nil",
	})
	// Nil registry: should return the original ctx unchanged, nil session,
	// and a no-op release.
	if egressCtx != ctx {
		t.Error("expected original context back for nil registry")
	}
	if sess != nil {
		t.Error("expected nil session for nil registry")
	}
	// release should not panic.
	release("done")
}

func TestRegisterEgressForceCloseViaContextCancel(t *testing.T) {
	t.Parallel()
	reg := newEgressRegistry()
	ctx := context.Background()

	// Register a session. The closer cancels the context.
	egressCtx, _, release := registerEgress(ctx, reg, egressSessionKindHTTP, EgressMeta{
		Provider:          "anthropic",
		UpstreamURL:       "https://api.anthropic.com/v1/messages",
		UpstreamRequestID: "",
		AttemptNo:         0,
		ParentRequestID:   "req-force",
	})
	defer release("cleanup")

	// Simulate a wedged provider call by draining the registry with a
	// very short deadline, which triggers force-close.
	drainCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := reg.Drain(drainCtx, "test.shutdown")

	// Force-close should have fired because the session was not released.
	if result.ForceClosed == 0 {
		t.Errorf("expected force-close to fire, got ForceClosed=%d", result.ForceClosed)
	}
	// The egress context should be canceled.
	select {
	case <-egressCtx.Done():
		// expected
	default:
		t.Error("egress context should be canceled after force-close")
	}
}

func TestRegisterEgressRetryAttemptParentLinkage(t *testing.T) {
	t.Parallel()
	reg := newEgressRegistry()
	ctx := context.Background()

	// Register the parent egress session.
	_, parentSess, releaseParent := registerEgress(ctx, reg, egressSessionKindWebsocket, EgressMeta{
		Provider:          "codex",
		UpstreamURL:       "wss://chatgpt.com/backend-api/codex/responses",
		UpstreamRequestID: "",
		AttemptNo:         0,
		ParentRequestID:   "req-retry",
	})
	defer releaseParent("parent.done")

	// Register attempt 1 as a nested child.
	_, attempt1, releaseAttempt1 := registerEgress(ctx, reg, egressSessionKindAttempt, EgressMeta{
		Provider:          "codex",
		UpstreamURL:       "wss://chatgpt.com/backend-api/codex/responses",
		UpstreamRequestID: "",
		AttemptNo:         1,
		ParentRequestID:   "req-retry",
	}, livetrack.WithParent(parentSess))
	defer releaseAttempt1("attempt1.done")

	if reg.Count() != 2 {
		t.Fatalf("expected 2 sessions (parent + attempt), got %d", reg.Count())
	}

	// The attempt should have the parent as its ParentID.
	if attempt1.ParentID != parentSess.ID {
		t.Errorf("attempt ParentID=%q, want %q", attempt1.ParentID, parentSess.ID)
	}

	// Children of the parent should include the attempt.
	children := reg.ChildrenOf(parentSess.ID)
	if len(children) != 1 || children[0] != attempt1.ID {
		t.Errorf("ChildrenOf parent: got %v, want [%s]", children, attempt1.ID)
	}
}
