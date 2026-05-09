package codex

import (
	"strings"

	"goodkind.io/clyde/internal/session"
)

// ThreadName maps Codex thread names onto Clyde's provider-neutral session
// naming contract.
type ThreadName struct {
	Name string
}

// GetName returns the current Codex thread name.
func (name ThreadName) GetName() string {
	return strings.TrimSpace(name.Name)
}

// GetDisplayTitle returns the exact human-visible Codex thread title.
func (name ThreadName) GetDisplayTitle() string {
	return strings.TrimSpace(name.Name)
}

// Rename returns the exact Clyde display name derived from the current Codex
// thread name, or "" when the name is absent or unusable.
func (name ThreadName) Rename(_ string, taken map[string]bool) string {
	candidate := session.UniqueDisplayName(name.GetName(), taken)
	if candidate == "" || session.ValidateDisplayName(candidate) != nil {
		return ""
	}
	return candidate
}
