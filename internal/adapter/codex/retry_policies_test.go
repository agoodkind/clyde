package codex

import (
	"slices"
	"testing"

	adapterretry "goodkind.io/clyde/internal/adapter/retry"
)

func TestAppendBuiltinCodexRetryPoliciesAppendsBothBuiltins(t *testing.T) {
	t.Parallel()

	policies := appendBuiltinCodexRetryPolicies(nil)
	if len(policies) != 2 {
		t.Fatalf("len=%d want 2", len(policies))
	}
	byName := map[string]adapterretry.Policy{}
	for _, policy := range policies {
		byName[policy.Name] = policy
	}

	overload, ok := byName[codexOverloadedRetryPolicyName]
	if !ok {
		t.Fatalf("missing %s", codexOverloadedRetryPolicyName)
	}
	if len(overload.Match.MessageSubstrings) == 0 {
		t.Fatalf("overload policy must keep its message-substring gate")
	}

	transport, ok := byName[codexTransportRetryPolicyName]
	if !ok {
		t.Fatalf("missing %s", codexTransportRetryPolicyName)
	}
	if !transport.Enabled || transport.MaxAttempts != 3 || transport.RetryWhenResponseStarted {
		t.Fatalf("transport policy fields unexpected: %+v", transport)
	}
	if len(transport.Match.MessageSubstrings) != 0 {
		t.Fatalf("transport policy must not gate on a message substring")
	}
	wantClasses := []string{"scanner_error", "websocket_error", "response_failed"}
	if !slices.Equal(transport.Match.ErrorClasses, wantClasses) {
		t.Fatalf("transport ErrorClasses=%v want %v", transport.Match.ErrorClasses, wantClasses)
	}
	wantOperations := []string{codexWebsocketRetryOperation, codexHTTPRetryOperation}
	if !slices.Equal(transport.Match.Operations, wantOperations) {
		t.Fatalf("transport Operations=%v want %v", transport.Match.Operations, wantOperations)
	}
}

func TestAppendBuiltinCodexRetryPoliciesLetsOperatorOverrideByName(t *testing.T) {
	t.Parallel()

	operator := adapterretry.Policy{
		Name:        codexTransportRetryPolicyName,
		Enabled:     true,
		MaxAttempts: 7,
	}
	policies := appendBuiltinCodexRetryPolicies([]adapterretry.Policy{operator})
	if len(policies) != 2 {
		t.Fatalf("len=%d want 2", len(policies))
	}
	// The operator policy stays first so it wins in Decide, and the
	// transport builtin is suppressed so it cannot shadow the override.
	if policies[0].Name != codexTransportRetryPolicyName || policies[0].MaxAttempts != 7 {
		t.Fatalf("operator policy not preserved: %+v", policies[0])
	}
	for _, policy := range policies {
		if policy.Name == codexTransportRetryPolicyName && policy.MaxAttempts != 7 {
			t.Fatalf("builtin transport policy was not suppressed: %+v", policy)
		}
	}
	// The overload builtin is still appended since it was not overridden.
	if policies[1].Name != codexOverloadedRetryPolicyName {
		t.Fatalf("overload builtin missing, got %+v", policies)
	}
}
