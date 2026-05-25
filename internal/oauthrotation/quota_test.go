package oauthrotation

import "testing"

func TestQuotaStatusString(t *testing.T) {
	tests := []struct {
		name string
		in   quotaStatus
		want string
	}{
		{name: "unknown_is_empty", in: quotaUnknown, want: ""},
		{name: "allowed_wire_value", in: quotaAllowed, want: "allowed"},
		{name: "allowed_warning_wire_value", in: quotaAllowedWarning, want: "allowed_warning"},
		{name: "rejected_wire_value", in: quotaRejected, want: "rejected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
