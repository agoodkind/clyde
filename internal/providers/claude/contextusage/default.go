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
// sessionRef argument is the Claude session UUID and WorkDir is the
// session's workspace root: the caller passes it through so the
// spawn anchors at the project directory, which is required for
// claude --resume to locate MCP servers, hooks, and settings.
//
// The generic RefreshHint is accepted for protocol completeness. The
// claude prober spawns a fresh /context probe on every call, so the
// hint is effectively a no-op today; the field exists so the daemon
// can wire `clyde compact --refresh` through without a follow-up
// interface change once a caching prober path is added.
func (claudeProber) Probe(ctx context.Context, sessionRef string, opts contextusage.ProbeOptions) (contextusage.Snapshot, error) {
	// ForkSession is intentionally false. With ForkSession=true the
	// probe spawns claude with `--fork-session --session-id <new-uuid>`
	// and claude treats the new uuid as a resume target it cannot find,
	// stderr: "No conversation found with session ID: <new-uuid>".
	// `--no-session-persistence` (set inside ProbeContextUsage) already
	// guarantees the probe leaves no side effects on the original
	// transcript, so a fork is not needed for safety.
	return ProbeContextUsage(ctx, ProbeOptions{
		SessionID:   sessionRef,
		WorkDir:     opts.WorkDir,
		Binary:      "",
		Timeout:     0,
		ForkSession: false,
	})
}

func init() {
	contextusage.Register(claudeProberID, claudeProber{})
}
