package session

import "strings"

// ProviderSessionName lets a provider expose a discovered upstream title.
// Rename remains for compatibility with providers that still know how to build
// legacy slug aliases, but session-domain adoption now prefers exact display
// names plus stable IDs.
type ProviderSessionName interface {
	GetName() string
	Rename(currentName string, taken map[string]bool) string
}

// ProviderSessionDisplayTitle lets a provider expose the exact human-visible
// title separately from any legacy slug alias.
type ProviderSessionDisplayTitle interface {
	GetDisplayTitle() string
}

// GetName returns the provider-observed session title, when one is available.
func (r DiscoveryResult) GetName() string {
	if r.NameContract == nil {
		return ""
	}
	return strings.TrimSpace(r.NameContract.GetName())
}

// DisplayTitle returns the exact provider-observed title when the provider can
// distinguish that from the legacy alias input.
func (r DiscoveryResult) DisplayTitle() string {
	if r.NameContract == nil {
		return ""
	}
	if display, ok := r.NameContract.(ProviderSessionDisplayTitle); ok {
		return strings.TrimSpace(display.GetDisplayTitle())
	}
	return r.GetName()
}

// Rename asks the provider-owned naming contract for the legacy registry alias,
// or "" when no compatibility alias is available.
func (r DiscoveryResult) Rename(currentName string, taken map[string]bool) string {
	if r.NameContract == nil {
		return ""
	}
	return strings.TrimSpace(r.NameContract.Rename(currentName, taken))
}
