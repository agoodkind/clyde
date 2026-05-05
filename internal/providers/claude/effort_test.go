package claude

import "testing"

func TestParseEffortLevelAllowsClaudeLevels(t *testing.T) {
	for _, raw := range []string{"", "low", "medium", "high", "max"} {
		if _, err := ParseEffortLevel(raw); err != nil {
			t.Fatalf("ParseEffortLevel(%q) returned error: %v", raw, err)
		}
	}
	got, err := ParseEffortLevel("max")
	if err != nil {
		t.Fatalf("ParseEffortLevel max: %v", err)
	}
	if got != EffortLevelMax {
		t.Fatalf("max parsed as %q", got)
	}
}

func TestParseEffortLevelRejectsCodexOnlyLevels(t *testing.T) {
	for _, raw := range []string{"none", "minimal", "xhigh", "HIGH"} {
		if _, err := ParseEffortLevel(raw); err == nil {
			t.Fatalf("ParseEffortLevel(%q) returned nil error", raw)
		}
	}
}
