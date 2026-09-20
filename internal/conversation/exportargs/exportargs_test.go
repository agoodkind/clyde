package exportargs

import "testing"

func TestDeclarationsNameEveryArgumentOnce(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, declaration := range Declarations() {
		if seen[declaration.Canonical] {
			t.Fatalf("duplicate argument %q", declaration.Canonical)
		}
		seen[declaration.Canonical] = true
	}
}
