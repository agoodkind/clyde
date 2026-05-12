package contextusage

import "sync"

// registry holds the provider-id -> Prober mapping. It is a process
// singleton so provider packages can register at init or
// constructor time without threading state through the dependency
// graph. The [sync.Map] shape allows concurrent reads from the
// compact engine and any other generic consumer.
var registry sync.Map

// Register associates a provider id with its Prober implementation.
// Callers from provider packages call this at startup (typically in
// an init or a NewDefault constructor). A subsequent Register for
// the same id replaces the previous entry, which keeps tests that
// inject fakes simple.
func Register(providerID string, prober Prober) {
	registry.Store(providerID, prober)
}

// Get returns the Prober registered for providerID, or false when
// no provider has registered. The compact engine calls this when it
// needs to refresh a Snapshot for a session whose provider id is
// known.
func Get(providerID string) (Prober, bool) {
	value, ok := registry.Load(providerID)
	if !ok {
		return nil, false
	}
	prober, ok := value.(Prober)
	if !ok {
		return nil, false
	}
	return prober, true
}
