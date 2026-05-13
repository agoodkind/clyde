// Package mitmcontrib hosts a provider-neutral registry for MITM
// launch-environment contributors. Each provider package registers a
// Contributor at init time so generic code in cmd/ and internal/daemon
// can iterate registered providers without importing provider packages
// or hard-coding env keys. The shape mirrors internal/contextusage.
package mitmcontrib

import (
	"context"
	"sync"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
)

// Contributor produces the MITM launch environment for a single
// provider. Implementations live in provider packages and own the
// vendor-specific env keys (ANTHROPIC_BASE_URL, OPENAI_BASE_URL,
// SSL_CERT_FILE, etc.) so the generic adapter and daemon code never
// import them directly.
//
// The interface accepts cfg and proxy because the contributor needs
// the adapter port and the live MITM proxy to construct correct
// values. ctx carries logging correlation.
type Contributor interface {
	// Env returns the env vars this provider wants installed in a
	// child process launch. The map is provider-owned: the caller
	// merges entries from every registered contributor without
	// inspecting keys.
	Env(ctx context.Context, cfg *config.Config, proxy *mitm.Proxy) (map[string]string, error)
}

// Sanitizer is an optional capability a Contributor may also
// implement. Callers that hand off an inherited env (e.g. cmd/ when
// forwarding to a child binary) type-assert to Sanitizer and apply
// each provider's stale-entry scrub before layering on fresh values.
// The split keeps Contributor minimal while letting providers own
// their stale-detection rules.
type Sanitizer interface {
	Sanitize(env []string) []string
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Contributor{}
)

// Register associates a provider id with its Contributor. A later
// Register call with the same id replaces the previous entry, which
// keeps tests that inject fakes simple.
func Register(providerID string, c Contributor) {
	registryMu.Lock()
	registry[providerID] = c
	registryMu.Unlock()
}

// Get returns the Contributor registered for providerID. The daemon's
// ProviderLaunchEnvironment dispatches by id; consumers that want all
// registered providers use Contributors.
func Get(providerID string) (Contributor, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	contributor, ok := registry[providerID]
	return contributor, ok
}

// Contributors returns every registered Contributor in unspecified
// order. Generic callers that need to layer env from every provider
// (e.g. cmd/root.go forwarding to a child binary) iterate this list.
func Contributors() []Contributor {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Contributor, 0, len(registry))
	for _, contributor := range registry {
		out = append(out, contributor)
	}
	return out
}

// IDs returns the provider ids that have a registered Contributor, in
// unspecified order. Callers that fetch env through the daemon RPC
// (and thus only need the provider id, not the in-process contributor
// implementation) iterate this list.
func IDs() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for id := range registry {
		out = append(out, id)
	}
	return out
}
