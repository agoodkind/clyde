package anthropic

import (
	"slices"
	"testing"
)

// TestSuppressBetaFlagsRemovesNamedFlagPreservingOrder asserts the
// surgical removal drops exactly the named flag and leaves the order and
// every other flag in the learned baseline untouched.
func TestSuppressBetaFlagsRemovesNamedFlagPreservingOrder(t *testing.T) {
	t.Parallel()
	const beta = "oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,thinking-token-count-2026-05-13,context-management-2025-06-27,claude-code-20250219"
	got, removed := suppressBetaFlags(beta, []string{"thinking-token-count-2026-05-13"})
	want := "oauth-2025-04-20,interleaved-thinking-2025-05-14,redact-thinking-2026-02-12,context-management-2025-06-27,claude-code-20250219"
	if got != want {
		t.Fatalf("suppressBetaFlags=%q want %q", got, want)
	}
	if !slices.Equal(removed, []string{"thinking-token-count-2026-05-13"}) {
		t.Fatalf("removed=%v want [thinking-token-count-2026-05-13]", removed)
	}
}

// TestSuppressBetaFlagsCaseInsensitiveAndMultiple asserts matching is
// case-insensitive and that multiple suppress entries all apply.
func TestSuppressBetaFlagsCaseInsensitiveAndMultiple(t *testing.T) {
	t.Parallel()
	const beta = "a-flag,B-Flag,c-flag"
	got, removed := suppressBetaFlags(beta, []string{"b-flag", "C-FLAG"})
	if got != "a-flag" {
		t.Fatalf("suppressBetaFlags=%q want %q", got, "a-flag")
	}
	if len(removed) != 2 {
		t.Fatalf("removed=%v want 2 entries", removed)
	}
}

// TestSuppressBetaFlagsAbsentFlagIsNoOp asserts a suppress entry that is
// not present leaves the header and removed list empty-changed.
func TestSuppressBetaFlagsAbsentFlagIsNoOp(t *testing.T) {
	t.Parallel()
	const beta = "oauth-2025-04-20,claude-code-20250219"
	got, removed := suppressBetaFlags(beta, []string{"not-present-2099-01-01"})
	if got != beta {
		t.Fatalf("suppressBetaFlags=%q want unchanged %q", got, beta)
	}
	if len(removed) != 0 {
		t.Fatalf("removed=%v want empty", removed)
	}
}

// TestSuppressBetaFlagsEmptyInputsAreNoOps guards the empty-string and
// empty-suppress fast paths.
func TestSuppressBetaFlagsEmptyInputsAreNoOps(t *testing.T) {
	t.Parallel()
	if got, removed := suppressBetaFlags("", []string{"x"}); got != "" || removed != nil {
		t.Fatalf("empty beta: got=%q removed=%v", got, removed)
	}
	if got, removed := suppressBetaFlags("a,b", nil); got != "a,b" || removed != nil {
		t.Fatalf("empty suppress: got=%q removed=%v", got, removed)
	}
}
