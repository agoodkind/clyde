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

func (name ThreadName) GetName() string {
	return strings.TrimSpace(name.Name)
}

func (name ThreadName) Rename(_ string, taken map[string]bool) string {
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
