package adapter

import (
	"sync"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
)

// ingressRegistry holds the typed, per-family IngressContract
// implementations the adapter dispatches against. Vendor packages
// populate this registry exactly once at process startup through the
// composition root (internal/adapter/ingress_registration.go); the
// boundary file (this file) holds no vendor import and constructs no
// vendor type. The shape mirrors boundaryRegistry from
// family_error_mapping.go so the layering is identical.
type ingressRegistry struct {
	mu        sync.RWMutex
	contracts map[ingresscontract.IngressFamily]ingresscontract.IngressContract
}

func newIngressRegistry() *ingressRegistry {
	return &ingressRegistry{
		mu:        sync.RWMutex{},
		contracts: map[ingresscontract.IngressFamily]ingresscontract.IngressContract{},
	}
}

// Register stores the contract for a family. Implements
// ingresscontract.IngressRegistrar so vendor packages can call
// Register without importing the adapter package.
func (r *ingressRegistry) Register(family ingresscontract.IngressFamily, contract ingresscontract.IngressContract) {
	if contract == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contracts[family] = contract
}

// contract returns the registered IngressContract for a family.
func (r *ingressRegistry) contract(family ingresscontract.IngressFamily) (ingresscontract.IngressContract, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.contracts[family]
	return c, ok
}

// defaultIngressRegistry is the package-level registry every
// non-Server caller reads from. Vendor packages register into it
// from internal/adapter/ingress_registration.go at init() time.
// The boundary file (this file) holds no vendor import; the
// registration file is the only place under
// internal/adapter/*.go (depth 1) that imports a vendor ingress
// package.
var defaultIngressRegistry = newIngressRegistry()

// activeIngressContract returns the IngressContract the dispatcher
// should use for a request. The Cursor BYOK ingress is the only
// vendor wired today; future vendors plug in by registering under
// their own family identifier without changing the dispatcher.
//
// Returns false when no ingress contract has been registered. The
// caller must treat that as an internal misconfiguration.
func activeIngressContract() (ingresscontract.IngressContract, bool) {
	return defaultIngressRegistry.contract(ingresscontract.IngressFamilyCursor)
}
