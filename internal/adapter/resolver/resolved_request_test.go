package resolver

import "testing"

func TestResolvedRequestProviderEffortUsesWireValueOrIdentity(t *testing.T) {
	tests := []struct {
		name string
		req  ResolvedRequest
		want Effort
	}{
		{name: "mapped", req: ResolvedRequest{Effort: "ultra", WireEffort: "max"}, want: "max"},
		{name: "identity", req: ResolvedRequest{Effort: "future-tier"}, want: "future-tier"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.req.ProviderEffort(); got != test.want {
				t.Fatalf("ProviderEffort() = %q, want %q", got, test.want)
			}
		})
	}
}
