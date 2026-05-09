package ui

import "testing"

// TestShortPathPure checks the current shortPath helper behavior against a
// stable HOME setting.
func TestShortPathPure(t *testing.T) {
	t.Setenv("HOME", "/Users/test")
	cases := []struct {
		name string
		root string
		want string
	}{
		{name: "empty root returns dash", root: "", want: "-"},
		{name: "root equals home collapses to tilde", root: "/Users/test", want: "~"},
		{name: "root under home gets relative tilde prefix", root: "/Users/test/Sites/repo", want: "~/Sites/repo"},
		{name: "root outside home is returned unchanged", root: "/var/log/clyde", want: "/var/log/clyde"},
		{name: "root shorter than home stays absolute", root: "/Users", want: "/Users"},
		{name: "home prefix without trailing slash does not match", root: "/Users/testing/Sites/repo", want: "/Users/testing/Sites/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shortPath(tc.root)
			if got != tc.want {
				t.Fatalf("shortPath(%q) = %q, want %q", tc.root, got, tc.want)
			}
		})
	}
}
