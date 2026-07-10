package resolver

import "testing"

func TestProviderIDString(t *testing.T) {
	cases := []struct {
		id   ProviderID
		want string
	}{
		{ProviderUnknown, ""},
		{ProviderAnthropic, "anthropic"},
		{ProviderCodex, "codex"},
	}
	for _, tc := range cases {
		if got := tc.id.String(); got != tc.want {
			t.Errorf("ProviderID(%v).String() = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestProviderIDValid(t *testing.T) {
	cases := []struct {
		id   ProviderID
		want bool
	}{
		{ProviderUnknown, false},
		{ProviderAnthropic, true},
		{ProviderCodex, true},
		{ProviderID("nonsense"), false},
	}
	for _, tc := range cases {
		if got := tc.id.Valid(); got != tc.want {
			t.Errorf("ProviderID(%v).Valid() = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestEffortString(t *testing.T) {
	cases := []struct {
		e    Effort
		want string
	}{
		{EffortUnset, ""},
		{EffortNone, "none"},
		{EffortLow, "low"},
		{EffortMedium, "medium"},
		{EffortHigh, "high"},
		{EffortXHigh, "xhigh"},
		{EffortMax, "max"},
	}
	for _, tc := range cases {
		if got := tc.e.String(); got != tc.want {
			t.Errorf("Effort(%q).String() = %q, want %q", tc.e, got, tc.want)
		}
	}
}

func TestEffortValid(t *testing.T) {
	known := []Effort{EffortUnset, EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}
	for _, e := range known {
		if !e.Valid() {
			t.Errorf("Effort(%q).Valid() = false, want true", e)
		}
	}
	if !Effort("future-provider-tier").Valid() {
		t.Errorf("future provider effort should be valid")
	}
	if !Effort(" future-provider-tier ").Valid() {
		t.Errorf("wildcard effort with surrounding whitespace should remain valid")
	}
}

func TestParseEffort(t *testing.T) {
	cases := []struct {
		raw    string
		want   Effort
		wantOK bool
	}{
		{"", EffortUnset, true},
		{"none", EffortNone, true},
		{"low", EffortLow, true},
		{"medium", EffortMedium, true},
		{"high", EffortHigh, true},
		{"xhigh", EffortXHigh, true},
		{"max", EffortMax, true},
		{"NONE", Effort("NONE"), true},
		{"medium ", Effort("medium "), true},
		{"unknown", Effort("unknown"), true},
	}
	for _, tc := range cases {
		got, ok := ParseEffort(tc.raw)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ParseEffort(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}
