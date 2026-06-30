package mitm

func (r *registry) unregister(id ProviderID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.providers[:0]
	for _, existing := range r.providers {
		if existing.ID() == id {
			continue
		}
		out = append(out, existing)
	}
	r.providers = out
}

// UnregisterProvider removes a Provider previously installed by a test.
func UnregisterProvider(id ProviderID) {
	defaultRegistry.unregister(id)
}

// RegisterProviderFirst installs a Provider before existing providers for tests.
func RegisterProviderFirst(p Provider) {
	if p == nil {
		return
	}
	defaultRegistry.registerFirst(p)
}

func (r *registry) registerFirst(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := p.ID()
	out := make([]Provider, 0, len(r.providers)+1)
	out = append(out, p)
	for _, existing := range r.providers {
		if existing.ID() == id {
			continue
		}
		out = append(out, existing)
	}
	r.providers = out
}
