package clispec

import (
	"testing"
)

// TestSplitRolesDropsEmptyEntries proves the comma-separated roles flag parses
// into a clean role set.
func TestSplitRolesDropsEmptyEntries(t *testing.T) {
	t.Parallel()

	roles := splitRoles(" user, ,assistant,")
	if len(roles) != 2 || roles[0] != "user" || roles[1] != "assistant" {
		t.Fatalf("roles = %v, want [user assistant]", roles)
	}
	if splitRoles("  ") != nil {
		t.Fatal("blank roles input did not return nil")
	}
}

// TestTimeBoundUnixParsesAcceptedLayouts proves the after and until flags
// accept the documented spellings, return zero for empty, and reject garbage.
func TestTimeBoundUnixParsesAcceptedLayouts(t *testing.T) {
	t.Parallel()

	zero, err := timeBoundUnix("", "after")
	if err != nil || zero != 0 {
		t.Fatalf("empty bound = (%d, %v), want (0, nil)", zero, err)
	}
	for _, spelling := range []string{"2026-06-09T12:00:00Z", "2026-06-09 12:00", "2026-06-09"} {
		parsed, parseErr := timeBoundUnix(spelling, "after")
		if parseErr != nil || parsed <= 0 {
			t.Fatalf("bound %q = (%d, %v), want a positive unix time", spelling, parsed, parseErr)
		}
	}
	if _, err := timeBoundUnix("next tuesday", "until"); err == nil {
		t.Fatal("garbage bound parsed without error")
	}
}
