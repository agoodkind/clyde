// Package discoveryroots hosts a provider-neutral registry for the
// filesystem roots a provider scans for adoptable sessions. Each
// provider package registers a Roots implementation at init so the
// daemon's discovery sweep no longer name-checks providers or imports
// vendor discovery packages directly. The shape mirrors
// internal/contextusage.
package discoveryroots

import "sync"

// Roots returns the filesystem roots a single provider wants the
// daemon to scan. Implementations live in provider discovery packages
// and own the vendor-specific path families (e.g. ~/.claude/projects,
// ~/.codex/sessions).
type Roots interface {
	// Paths returns the absolute roots to walk for the user whose
	// home directory is home. An empty result is valid when the
	// provider has nothing to scan on this host.
	Paths(home string) []string
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Roots{}
)

// Register associates a provider id with its Roots implementation. A
// later Register call with the same id replaces the previous entry.
func Register(providerID string, r Roots) {
	registryMu.Lock()
	registry[providerID] = r
	registryMu.Unlock()
}

// All returns every registered Roots in unspecified order. The
// daemon's discovery sweep iterates this list to assemble the full
// set of paths to scan across every registered provider.
func All() []Roots {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Roots, 0, len(registry))
	for _, roots := range registry {
		out = append(out, roots)
	}
	return out
}
