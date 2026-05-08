package ingresscontract

import "testing"

// TestPathKindConstantsStable pins the primitive request-path string
// values the adapter dispatcher compares against. Changing these
// values would silently miscount or skip vendor-specific paths in
// the generic dispatcher; the test is here so the boundary contract
// owns the comparison vocabulary instead of a vendor enum.
func TestPathKindConstantsStable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "foreground", got: PathKindForeground, want: "foreground"},
		{name: "background", got: PathKindBackground, want: "background"},
		{name: "resume", got: PathKindResume, want: "resume"},
		{name: "subagent", got: PathKindSubagent, want: "subagent"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestIngressFamilyCursorString pins the canonical Cursor family
// identifier so the registration root and the registry agree on the
// lookup key.
func TestIngressFamilyCursorString(t *testing.T) {
	t.Parallel()
	if string(IngressFamilyCursor) != "cursor" {
		t.Fatalf("IngressFamilyCursor = %q want cursor", IngressFamilyCursor)
	}
}
