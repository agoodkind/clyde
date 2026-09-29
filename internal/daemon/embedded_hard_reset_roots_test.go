package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

// TestHardResetPlanListsSemanticStateOnlyWhenPresent prints the default hard
// reset plan without and then with the conversation-semantic state directory.
// The preserved roots must list that directory only when it exists. With the
// directory present, a reset target inside it must be refused as an overlap
// with a protected directory.
func TestHardResetPlanListsSemanticStateOnlyWhenPresent(t *testing.T) {
	resetTestRoots(t)
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	semanticState := filepath.Join(config.DefaultStateDir(), "conversation-semantic")
	resolved, err := resolvedResetPath(semanticState)
	if err != nil {
		t.Fatal(err)
	}
	for _, present := range []bool{false, true} {
		if present {
			if err := os.MkdirAll(semanticState, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		targets, err := hardResetTargetsForScope(t.Context(), cfg, HardResetScopeAll)
		if err != nil {
			t.Fatalf("present %t: inventory: %v", present, err)
		}
		var plan bytes.Buffer
		if err := printResetPlan(t.Context(), &plan, targets, cfg); err != nil {
			t.Fatalf("present %t: print plan: %v", present, err)
		}
		preserved := strings.SplitN(plan.String(), "Clyde hard reset preserved roots:", 2)[1]
		if listed := strings.Contains(preserved, "  "+resolved+"\n"); listed != present {
			t.Fatalf("present %t: preserved roots list %s = %t:\n%s", present, resolved, listed, plan.String())
		}
	}
	inside := resetFileTarget(filepath.Join(semanticState, "outbox-pool.sqlite"), config.DefaultStateDir())
	if err := validateResetTarget(t.Context(), inside, cfg); err == nil || !strings.Contains(err.Error(), "protected directory") {
		t.Fatalf("reset target inside the semantic state directory error = %v, want a protected directory refusal", err)
	}
}
