package daemon

import (
	"testing"
	"time"

	"goodkind.io/clyde/internal/livetrack"
)

// TestBudgetProfilesMatchLegacyConstants asserts the named budgets reproduce
// the pre-refactor timing exactly, so Quiesce drains on the same schedule the
// hand-sequenced path did.
func TestBudgetProfilesMatchLegacyConstants(t *testing.T) {
	if budgetReload != (livetrack.Budget{Cap: 60 * time.Second, IdleGrace: 5 * time.Second}) {
		t.Errorf("budgetReload = %+v, want cap 60s grace 5s", budgetReload)
	}
	if budgetShutdown != (livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0}) {
		t.Errorf("budgetShutdown = %+v, want cap 5s grace 0", budgetShutdown)
	}
}
