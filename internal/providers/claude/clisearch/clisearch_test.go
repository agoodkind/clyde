package clisearch

import (
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/search"
)

// TestInitRegistersClaudeBackend asserts importing this package registers the
// "claude" search backend, so search.NewClientForModel resolves to this
// package's client rather than the unregistered-backend error client.
func TestInitRegistersClaudeBackend(t *testing.T) {
	var local config.SearchLocal
	got := search.NewClientForModel(config.SearchConfig{Backend: backendName, Local: local}, "haiku")
	if _, ok := got.(*client); !ok {
		t.Fatalf("claude backend not registered; client type = %T", got)
	}
}

// TestCleanModelArg covers the default, trim, and NUL-rejection behavior moved
// from the search package.
func TestCleanModelArg(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "empty defaults to haiku", in: "", want: defaultModel, wantErr: false},
		{name: "whitespace defaults to haiku", in: "   ", want: defaultModel, wantErr: false},
		{name: "trims surrounding space", in: "  sonnet  ", want: "sonnet", wantErr: false},
		{name: "passes a normal model", in: "haiku", want: "haiku", wantErr: false},
		{name: "rejects a NUL byte", in: "bad\x00model", want: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cleanModelArg(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("cleanModelArg(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
