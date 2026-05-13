// Package categorystyle hosts a provider-neutral registry for the
// per-category color hints a provider wants the dashboard to use when
// rendering /context categories. Each provider package registers its
// color mapping at init so consumers (TUI, web dashboard) can look up
// colors by (providerID, categoryName) without importing vendor
// packages. The shape mirrors internal/contextusage.
package categorystyle

import (
	"maps"
	"sync"
)

// Color is the provider-supplied color hint for a single category.
// The string value is whatever the consumer expects (today: a CSS
// color token or a tcell color name); typing it keeps boundaries
// explicit and lets future changes happen here without rippling.
type Color string

var (
	registryMu sync.RWMutex
	registry   = map[string]map[string]Color{}
)

// Register associates a provider id with its (categoryName -> Color)
// mapping. A later Register call with the same id replaces the
// previous entry. Providers that have no concrete colors yet should
// register an empty map so consumers can still resolve to the
// "registered but empty" state.
func Register(providerID string, mapping map[string]Color) {
	owned := make(map[string]Color, len(mapping))
	maps.Copy(owned, mapping)
	registryMu.Lock()
	registry[providerID] = owned
	registryMu.Unlock()
}

// ColorFor returns the registered color for (providerID, name) and
// whether one was found. Callers fall back to consumer-side defaults
// when the second return is false.
func ColorFor(providerID, name string) (Color, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	mapping, ok := registry[providerID]
	if !ok {
		return "", false
	}
	color, ok := mapping[name]
	return color, ok
}
