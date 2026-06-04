package conversation

import (
	"errors"
	"sync"

	"goodkind.io/clyde/internal/providerid"
)

// ErrProviderNotRegistered is returned by [Registry.Lookup] when no parser is
// bound for the requested provider.
var ErrProviderNotRegistered = errors.New("conversation: provider not registered")

// Registry holds the typed map of [providerid.Provider] to [Parser]. It is
// constructed once at daemon and CLI startup and shared across requests.
// Lookups are concurrent-safe; registration is serialized.
type Registry struct {
	mu      sync.RWMutex
	parsers map[providerid.Provider]Parser
}

// NewRegistry returns an empty Registry. Callers register the live parsers
// during daemon and CLI construction.
func NewRegistry() *Registry {
	return &Registry{
		mu:      sync.RWMutex{},
		parsers: make(map[providerid.Provider]Parser),
	}
}

// Register adds a parser to the registry under its declared provider.
// Registering a second parser for a provider overwrites the first. The caller is
// expected to register exactly once per startup. A nil parser or one with an
// invalid provider is ignored.
func (r *Registry) Register(p Parser) {
	if p == nil {
		return
	}
	provider := p.Provider()
	if !provider.Valid() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parsers[provider] = p
}

// Lookup returns the parser registered for the given provider, or
// [ErrProviderNotRegistered] when no parser is bound. Lookups are
// concurrent-safe.
func (r *Registry) Lookup(provider providerid.Provider) (Parser, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.parsers[provider]
	if !ok {
		return nil, ErrProviderNotRegistered
	}
	return p, nil
}

// Providers returns the registered provider identities, useful for startup
// logging and tests.
func (r *Registry) Providers() []providerid.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]providerid.Provider, 0, len(r.parsers))
	for provider := range r.parsers {
		out = append(out, provider)
	}
	return out
}
