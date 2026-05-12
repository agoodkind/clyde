package contextusage

import (
	"context"

	"goodkind.io/clyde/internal/contextusage"
)

// claudeProberID names this provider in the generic contextusage
// registry. The constant lives here rather than in the Claude
// provider's identity package because that package is currently
// unaware of generic context-usage routing.
const claudeProberID = "claude"

// claudeProber adapts the Claude-specific ProbeContextUsage spawn
// machinery to the provider-neutral Prober contract. Callers in
// generic code reach the spawn through Get(claudeProberID).
type claudeProber struct{}

// Probe satisfies the generic contextusage.Prober interface. The
// sessionRef argument is the Claude session UUID; WorkDir defaults
// to the empty string here because the generic caller does not own
// per-session workspace state.
//
// The generic RefreshHint is accepted for protocol completeness. The
// claude prober spawns a fresh /context probe on every call, so the
// hint is effectively a no-op today; the field exists so the daemon
// can wire `clyde compact --refresh` through without a follow-up
// interface change once a caching prober path is added.
func (claudeProber) Probe(ctx context.Context, sessionRef string, _ contextusage.ProbeOptions) (contextusage.Snapshot, error) {
	return ProbeContextUsage(ctx, ProbeOptions{
		SessionID:   sessionRef,
		WorkDir:     "",
		Binary:      "",
		Timeout:     0,
		ForkSession: true,
	})
}

func init() {
	contextusage.Register(claudeProberID, claudeProber{})
}
