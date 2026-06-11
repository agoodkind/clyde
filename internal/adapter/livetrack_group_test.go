package adapter

import (
	"log/slog"
	"testing"

	"goodkind.io/clyde/internal/livetrack"
)

// TestAdapterRegistriesAttachToGroup asserts the ingress and egress registries
// attach to the supplied group as a quiet-relevant PhaseIngress and PhaseEgress
// member respectively, so the daemon's Quiesce drains them in order and an
// in-flight request or provider call holds quiet.
func TestAdapterRegistriesAttachToGroup(t *testing.T) {
	t.Parallel()
	g := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	before := g.MemberCount()

	_ = newAdapterIngressRegistry(g, slog.Default())
	_ = newEgressRegistry(g)

	if g.MemberCount() != before+2 {
		t.Fatalf("expected 2 attached members (ingress, egress), got %d", g.MemberCount()-before)
	}
}
