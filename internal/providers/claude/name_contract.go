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

// GetDisplayTitle returns the exact human-visible Claude custom title.
func (name CustomTitleName) GetDisplayTitle() string {
	return name.GetName()
}

// Rename returns the exact Clyde session name derived from the current Claude
// custom title, or "" when the title is absent or unusable.
func (name CustomTitleName) Rename(_ string, taken map[string]bool) string {
	candidate := session.UniqueDisplayName(name.GetName(), taken)
	if candidate == "" || session.ValidateDisplayName(candidate) != nil {
		return ""
	}
	return candidate
}
