package contextusage

import "context"

// Source tags where a Snapshot came from so consumers (loggers,
// tests, the daemon cache) can tell a fresh probe result apart from
// a cache hit. The constants live in the generic package because
// every provider implementation shares the same three layers.
type Source string

// Source constants cover the three places a Snapshot can originate.
// A caller that wants to force freshness suppresses both cache tiers
// at the provider layer.
const (
	// SourceProbe is a fresh provider-native probe of /context.
	SourceProbe Source = "probe"
	// SourceCacheMem is a hit in the in-process cache.
	SourceCacheMem Source = "cache_mem"
	// SourceCacheDisk is a hit in the on-disk per-session cache.
	SourceCacheDisk Source = "cache_disk"
)

// ProbeOptions carries generic, provider-neutral hints for a single
// Probe call. The struct is intentionally small: provider-specific
// knobs stay inside the provider implementation. Adding a new field
// here means every Prober implementation can opt into honoring it
// without widening the interface signature again.
type ProbeOptions struct {
	// RefreshHint asks the implementation to bypass any cached
	// snapshot and force a fresh provider-native probe. Implementations
	// that do not cache treat it as a no-op.
	RefreshHint bool

	// WorkDir is the filesystem anchor for the spawn. Provider
	// implementations that run a sidecar process (claude --resume,
	// codex resume, etc.) set the spawn's cwd to this value so the
	// sidecar can find the project's MCP servers, hooks, and settings.
	// Callers pass the session's workspace root. An empty string lets
	// the implementation inherit its parent process cwd, which is
	// usually wrong for daemon-spawned probes.
	WorkDir string
}

// Prober is the generic contract the compact engine and any other
// provider-neutral consumer relies on to obtain a Snapshot for a
// given provider-native session id. Implementations live in provider
// packages and register themselves through Register at startup. The
// sessionRef argument is the provider's own session identifier; the
// generic caller passes it opaquely.
type Prober interface {
	Probe(ctx context.Context, sessionRef string, opts ProbeOptions) (Snapshot, error)
}
