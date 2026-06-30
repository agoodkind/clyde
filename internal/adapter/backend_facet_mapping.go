package adapter

import (
	"sync"

	"goodkind.io/clyde/internal/adapter/backendfacet"
	"goodkind.io/clyde/internal/logevent"
)

// backendFacetRegistry holds backend-owned request-story facet
// factories. Provider packages populate this registry through the
// adapter composition root, so request logging does not import
// provider facet types.
type backendFacetRegistry struct {
	mu        sync.RWMutex
	factories map[string]backendfacet.Factory
}

func newBackendFacetRegistry() *backendFacetRegistry {
	return &backendFacetRegistry{
		mu:        sync.RWMutex{},
		factories: map[string]backendfacet.Factory{},
	}
}

// Register stores the facet factory for a backend. It implements
// backendfacet.Registrar.
func (r *backendFacetRegistry) Register(backend string, factory backendfacet.Factory) {
	if backend == "" || factory == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[backend] = factory
}

func (r *backendFacetRegistry) requestFacet(backend string, input backendfacet.Input) logevent.Facet {
	r.mu.RLock()
	factory := r.factories[backend]
	r.mu.RUnlock()
	if factory == nil {
		return nil
	}
	return factory.RequestFacet(input)
}

var defaultBackendFacetRegistry = newBackendFacetRegistry()
