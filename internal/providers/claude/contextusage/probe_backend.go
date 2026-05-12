package contextusage

import (
	"context"
	"time"

	"goodkind.io/clyde/internal/contextusage"
)

// defaultProbeTimeout bounds a cold probe. Claude plus MCP servers
// typically wake in 15-30 seconds; 90 gives headroom for large
// workspaces without letting a hung process block forever.
const defaultProbeTimeout = 90 * time.Second

// probeBackend spawns claude with --resume and issues a
// get_context_usage control request. It is the truth source for
// Layer.Usage; every other retrieval path ultimately points here.
type probeBackend struct {
	sessionID string
	workDir   string
	timeout   time.Duration

	// probe is injected so tests can substitute a deterministic
	// response without spawning a claude binary.
	probe func(ctx context.Context, opts ProbeOptions) (contextusage.Snapshot, error)
}

// newProbeBackend builds a backend bound to one session. Tests call
// withProbeFn to swap the real ProbeContextUsage for a fixture.
func newProbeBackend(sessionID, workDir string) *probeBackend {
	return &probeBackend{
		sessionID: sessionID,
		workDir:   workDir,
		timeout:   defaultProbeTimeout,
		probe:     ProbeContextUsage,
	}
}

// Fetch runs the probe and returns the raw Snapshot. The layer
// wraps this with CapturedAt and Source=SourceProbe before returning
// to callers and before writing to cache.
func (p *probeBackend) Fetch(ctx context.Context) (contextusage.Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	started := currentTime()
	sessionContextLog.Logger().DebugContext(ctx, "session.context.probe.started",
		"component", "contextusage",
		"subcomponent", "probe",
		"session_id", p.sessionID,
		"work_dir", p.workDir,
		"timeout_s", int(p.timeout.Seconds()),
	)
	usage, err := p.probe(ctx, ProbeOptions{
		SessionID:   p.sessionID,
		WorkDir:     p.workDir,
		Binary:      "",
		Timeout:     p.timeout,
		ForkSession: true,
	})
	duration := time.Since(started)
	if err != nil {
		sessionContextLog.Logger().WarnContext(ctx, "session.context.probe.failed",
			"component", "contextusage",
			"subcomponent", "probe",
			"session_id", p.sessionID,
			"duration_ms", duration.Milliseconds(),
			"err", err,
		)
		return contextusage.Snapshot{}, err
	}
	sessionContextLog.Logger().InfoContext(ctx, "session.context.probe.completed",
		"component", "contextusage",
		"subcomponent", "probe",
		"session_id", p.sessionID,
		"duration_ms", duration.Milliseconds(),
		"total_tokens", usage.TotalTokens,
		"max_tokens", usage.MaxTokens,
		"percentage", usage.Percentage,
		"model", usage.Model,
		"categories", len(usage.Categories),
	)
	return usage, nil
}
