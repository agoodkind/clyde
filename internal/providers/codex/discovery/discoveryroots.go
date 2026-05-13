package discovery

import "goodkind.io/clyde/internal/providers/discoveryroots"

// codexRootsID names this provider in the discoveryroots registry.
const codexRootsID = "codex"

type codexRoots struct{}

// Paths returns Codex's adoptable-session root family. Today that is
// the user's ~/.codex directory; the slice shape leaves room for
// additional roots without changing the contract.
func (codexRoots) Paths(home string) []string {
	if home == "" {
		return nil
	}
	return []string{defaultCodexHome(home)}
}

func init() {
	discoveryroots.Register(codexRootsID, codexRoots{})
}
