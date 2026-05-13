package session

import "strings"

// ProviderSessionName lets a provider expose a discovered upstream title and
// project it into Clyde's exact human-visible session-name policy.
type ProviderSessionName interface {
	GetName() string
	Rename(currentName string, taken map[string]bool) string
}

// ProviderSessionTitle lets a provider expose the exact human-visible
// title separately from any boundary-specific fallback alias.
type ProviderSessionTitle interface {
	GetTitle() string
}

// GetName returns the provider-observed session title, when one is available.
func (r DiscoveryResult) GetName() string {
	if r.NameContract == nil {
		return ""
	}
	return strings.TrimSpace(r.NameContract.GetName())
}

// Title returns the exact provider-observed title when the provider can
// distinguish that from the legacy alias input.
func (r DiscoveryResult) Title() string {
	if r.NameContract == nil {
		return ""
	}
	if display, ok := r.NameContract.(ProviderSessionTitle); ok {
		return strings.TrimSpace(display.GetTitle())
	}
	return r.GetName()
}

// Rename asks the provider-owned naming contract for an exact Clyde session
// name, or "" when no provider-owned name is available.
func (r DiscoveryResult) Rename(currentName string, taken map[string]bool) string {
	if r.NameContract == nil {
		return ""
	}
	return strings.TrimSpace(r.NameContract.Rename(currentName, taken))
}
