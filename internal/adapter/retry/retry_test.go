package retry

import (
	"testing"
	"time"
)

func TestPolicyDelayForAttemptAppliesBoundedJitter(t *testing.T) {
	t.Parallel()

	policy := Policy{
		InitialDelay:   100 * time.Millisecond,
		MaxDelay:       500 * time.Millisecond,
		Multiplier:     2,
		JitterFraction: 0.25,
	}

	low := policy.DelayForAttempt(2, func() float64 { return 0 })
	if low != 150*time.Millisecond {
		t.Fatalf("low jitter delay=%s want 150ms", low)
	}
	high := policy.DelayForAttempt(2, func() float64 { return 1 })
	if high != 250*time.Millisecond {
		t.Fatalf("high jitter delay=%s want 250ms", high)
	}
	capped := policy.DelayForAttempt(5, func() float64 { return 1 })
	if capped != 500*time.Millisecond {
		t.Fatalf("capped delay=%s want 500ms", capped)
	}
}

func TestDecideUsesMaxAttemptsAsTotalAttempts(t *testing.T) {
	t.Parallel()

	policies := []Policy{{
		Name:         "test",
		Enabled:      true,
		MaxAttempts:  3,
		InitialDelay: time.Millisecond,
		Match: Matchers{
			Backends: []string{"codex"},
		},
	}}
	signal := Signal{Backend: "codex"}

	first := Decide(policies, signal, 1, nil)
	if !first.Retry {
		t.Fatalf("attempt 1 retry=%v reason=%q want retry", first.Retry, first.Reason)
	}
	second := Decide(policies, signal, 2, nil)
	if !second.Retry {
		t.Fatalf("attempt 2 retry=%v reason=%q want retry", second.Retry, second.Reason)
	}
	third := Decide(policies, signal, 3, nil)
	if third.Retry || third.Reason != "max_attempts_exhausted" {
		t.Fatalf("attempt 3 decision=%+v want exhausted", third)
	}
}

func TestPolicyMatchersRequireConfiguredPredicates(t *testing.T) {
	t.Parallel()

	policy := Policy{
		Name:    "test",
		Enabled: true,
		Match: Matchers{
			Backends:          []string{"codex"},
			Operations:        []string{"codex.responses.websocket.generate"},
			Statuses:          []int{503},
			ErrorClasses:      []string{"scanner_error"},
			ErrorCodes:        []string{"overloaded"},
			MessageSubstrings: []string{"overloaded"},
		},
	}
	matching := Signal{
		Backend:    "codex",
		Operation:  "codex.responses.websocket.generate",
		Status:     503,
		ErrorClass: "scanner_error",
		ErrorCode:  "overloaded",
		Message:    "server overloaded",
	}
	if !policy.Matches(matching) {
		t.Fatalf("policy should match signal")
	}
	matching.ErrorClass = "context_length_exceeded"
	if policy.Matches(matching) {
		t.Fatalf("policy should reject mismatched error class")
	}
}

func TestDecideRejectsRetryAfterResponseStartedByDefault(t *testing.T) {
	t.Parallel()

	policies := []Policy{{
		Name:         "test",
		Enabled:      true,
		MaxAttempts:  3,
		InitialDelay: time.Millisecond,
		Match:        Matchers{Backends: []string{"codex"}},
	}}
	decision := Decide(policies, Signal{Backend: "codex", ResponseStarted: true}, 1, nil)
	if decision.Retry || decision.Reason != "response_started" {
		t.Fatalf("decision=%+v want response_started without retry", decision)
	}
}
