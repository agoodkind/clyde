package wireflags

import (
	"slices"
	"testing"
)

// TestSuppressRemovesNamedTokenPreservingOrder asserts the surgical removal
// drops exactly the named token and leaves the order and every other token
// untouched.
func TestSuppressRemovesNamedTokenPreservingOrder(t *testing.T) {
	t.Parallel()
	const value = "alpha-flag,beta-flag,gamma-flag,delta-flag,epsilon-flag"
	got, removed := Suppress(value, []string{"gamma-flag"})
	want := "alpha-flag,beta-flag,delta-flag,epsilon-flag"
	if got != want {
		t.Fatalf("Suppress=%q want %q", got, want)
	}
	if !slices.Equal(removed, []string{"gamma-flag"}) {
		t.Fatalf("removed=%v want [gamma-flag]", removed)
	}
}

// TestSuppressCaseInsensitiveAndMultiple asserts matching is case-insensitive
// and that multiple drop entries all apply.
func TestSuppressCaseInsensitiveAndMultiple(t *testing.T) {
	t.Parallel()
	const value = "a-flag,B-Flag,c-flag"
	got, removed := Suppress(value, []string{"b-flag", "C-FLAG"})
	if got != "a-flag" {
		t.Fatalf("Suppress=%q want %q", got, "a-flag")
	}
	if len(removed) != 2 {
		t.Fatalf("removed=%v want 2 entries", removed)
	}
}

// TestSuppressAbsentTokenIsNoOp asserts a drop entry that is not present leaves
// the value and removed list unchanged.
func TestSuppressAbsentTokenIsNoOp(t *testing.T) {
	t.Parallel()
	const value = "alpha-flag,beta-flag"
	got, removed := Suppress(value, []string{"not-present-flag"})
	if got != value {
		t.Fatalf("Suppress=%q want unchanged %q", got, value)
	}
	if len(removed) != 0 {
		t.Fatalf("removed=%v want empty", removed)
	}
}

// TestSuppressEmptyInputsAreNoOps guards the empty-value and empty-drop fast
// paths.
func TestSuppressEmptyInputsAreNoOps(t *testing.T) {
	t.Parallel()
	if got, removed := Suppress("", []string{"x"}); got != "" || removed != nil {
		t.Fatalf("empty value: got=%q removed=%v", got, removed)
	}
	if got, removed := Suppress("a,b", nil); got != "a,b" || removed != nil {
		t.Fatalf("empty drop: got=%q removed=%v", got, removed)
	}
}
