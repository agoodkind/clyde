package claude

import (
	"strings"

	"goodkind.io/clyde/internal/session"
)

// CustomTitleName maps Claude's native custom-title records onto Clyde's
// provider-neutral session naming contract.
type CustomTitleName struct {
	Title string
}

// GetName returns the current Claude custom title, trimmed for display use.
func (name CustomTitleName) GetName() string {
	return strings.TrimSpace(name.Title)
}

// Rename returns the registry-safe Clyde session name derived from the current
// Claude custom title, or "" when the title is absent or unusable.
func (name CustomTitleName) Rename(_ string, taken map[string]bool) string {
	sanitized := session.Sanitize(name.GetName())
	if sanitized == "" {
		return ""
	}
	candidate := session.UniqueName(sanitized, taken)
	if candidate == "" || session.ValidateName(candidate) != nil {
		return ""
	}
	return candidate
}
