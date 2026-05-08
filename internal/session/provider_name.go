package session

import "strings"

// ProviderSessionName lets a provider own how a discovered upstream title maps
// to Clyde's display title and optional registry rename.
type ProviderSessionName interface {
	GetName() string
	Rename(currentName string, taken map[string]bool) string
}

// GetName returns the provider-observed session title, when one is available.
func (r DiscoveryResult) GetName() string {
	if r.NameContract == nil {
		return ""
	}
	return strings.TrimSpace(r.NameContract.GetName())
}

// Rename asks the provider-owned naming contract for the registry name Clyde
// should use for this discovery result, or "" when no rename should occur.
func (r DiscoveryResult) Rename(currentName string, taken map[string]bool) string {
	if r.NameContract == nil {
		return ""
	}
	return strings.TrimSpace(r.NameContract.Rename(currentName, taken))
}
