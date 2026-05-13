// Package categorystyle hosts a provider-neutral registry for category colors.
package categorystyle

import (
	"maps"
	"sync"
)

// Color is the provider-supplied color hint for a single category.
type Color string

var (
	registryMu sync.RWMutex
	registry   = map[string]map[string]Color{}
)

// Register associates a provider id with its category color mapping.
func Register(providerID string, mapping map[string]Color) {
	owned := make(map[string]Color, len(mapping))
	maps.Copy(owned, mapping)
	registryMu.Lock()
	registry[providerID] = owned
	registryMu.Unlock()
}

// ColorFor returns the registered color for a category.
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
