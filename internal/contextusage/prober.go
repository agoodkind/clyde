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

// Prober is the generic contract the compact engine and any other
// provider-neutral consumer relies on to obtain a Snapshot for a
// given provider-native session id. Implementations live in provider
// packages and register themselves through Register at startup. The
// sessionRef argument is the provider's own session identifier; the
// generic caller passes it opaquely.
type Prober interface {
	Probe(ctx context.Context, sessionRef string) (Snapshot, error)
}
