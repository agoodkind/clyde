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
// sessionRef argument is the Claude session UUID, WorkDir is the
// session's workspace root, and Model is the model name claude
// should pin so the Snapshot's MaxTokens matches the planner's
// target budget. The probe hands all three to claude on argv as
// `--resume`, `cmd.Dir`, and `--model`.
//
// The generic RefreshHint is accepted for protocol completeness.
// The claude prober spawns a fresh /context probe on every call, so
// the hint is effectively a no-op today; the field exists so the
// daemon can wire `clyde compact --refresh` through without a
// follow-up interface change once a caching prober path is added.
func (claudeProber) Probe(ctx context.Context, sessionRef string, opts contextusage.ProbeOptions) (contextusage.Snapshot, error) {
	return ProbeContextUsage(ctx, ProbeOptions{
		SessionID: sessionRef,
		WorkDir:   opts.WorkDir,
		Model:     opts.Model,
		Binary:    "",
		Timeout:   0,
	})
}

func init() {
	contextusage.Register(claudeProberID, claudeProber{})
}
