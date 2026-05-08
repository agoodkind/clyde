package session

import "strings"

// ProviderSessionName lets a provider own how a discovered upstream title maps
// to Clyde's display title and optional registry rename.
type ProviderSessionName interface {
	GetName() string
	Rename(currentName string, taken map[string]bool) string
}

// ProviderSessionDisplayTitle lets a provider expose the exact human-visible
// title separately from any sanitized Clyde registry alias.
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
// distinguish that from the registry-safe alias input.
func (r DiscoveryResult) DisplayTitle() string {
	if r.NameContract == nil {
		return ""
	}
	if display, ok := r.NameContract.(ProviderSessionDisplayTitle); ok {
		return strings.TrimSpace(display.GetDisplayTitle())
	}
	return r.GetName()
}

// Rename asks the provider-owned naming contract for the registry name Clyde
// should use for this discovery result, or "" when no rename should occur.
func (r DiscoveryResult) Rename(currentName string, taken map[string]bool) string {
	if r.NameContract == nil {
		return ""
	}
	return strings.TrimSpace(r.NameContract.Rename(currentName, taken))
}
