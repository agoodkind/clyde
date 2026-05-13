package discovery

import "goodkind.io/clyde/internal/providers/discoveryroots"

// claudeRootsID names this provider in the discoveryroots registry.
const claudeRootsID = "claude"

type claudeRoots struct{}

// Paths returns Claude's adoptable-session root family. Today that is
// a single directory under the user's home; the slice shape leaves
// room for additional roots without changing the contract.
func (claudeRoots) Paths(home string) []string {
	if home == "" {
		return nil
	}
	return []string{ProjectsRoot(home)}
}

func init() {
	discoveryroots.Register(claudeRootsID, claudeRoots{})
}
