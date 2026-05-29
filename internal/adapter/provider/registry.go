package provider

import (
	"sync"

	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// Registry holds the typed map of ProviderID to Provider. It is
// constructed once at daemon startup and shared across requests.
// Lookups are concurrent-safe; registration is serialized.
type Registry struct {
	mu        sync.RWMutex
	providers map[adapterresolver.ProviderID]Provider
}

// NewRegistry returns an empty Registry. Callers register the live
// providers during adapter construction.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[adapterresolver.ProviderID]Provider), mu: sync.
				RWMutex{},
	}
}

// Register adds a provider to the registry under its declared ID.
// Registering a second provider for an ID overwrites the first. The
// caller is expected to register exactly once per daemon startup.
func (r *Registry) Register(p Provider) {
	if p == nil {
		return
	}
	id := p.ID()
	if !id.Valid() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[id] = p
}

// Lookup returns the Provider registered for the given ID, or
// ErrProviderNotRegistered when no provider is bound. Lookups are
// concurrent-safe.
func (r *Registry) Lookup(id adapterresolver.ProviderID) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	if !ok {
		return nil, ErrProviderNotRegistered
	}
	return p, nil
}

// IDs returns the registered provider identities. Useful for daemon
// startup logging and tests.
func (r *Registry) IDs() []adapterresolver.ProviderID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]adapterresolver.ProviderID, 0, len(r.providers))
	for id := range r.providers {
		out = append(out, id)
	}
	return out
}
