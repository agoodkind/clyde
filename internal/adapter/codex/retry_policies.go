package codex

import (
	"strings"
	"time"

	adapterretry "goodkind.io/clyde/internal/adapter/retry"
)

// Codex retry operation names. The transport layer tags each retry
// signal with one of these so a policy can target a specific transport.
const (
	codexWebsocketRetryOperation = "codex.responses.websocket.generate"
	codexHTTPRetryOperation      = "codex.responses.http.generate"
)

// Builtin Codex retry policy names. An operator policy of the same name
// in adapter.retry.policies suppresses the matching builtin, so a
// config override by name wins.
const (
	codexOverloadedRetryPolicyName = "codex.responses.overloaded"
	codexTransportRetryPolicyName  = "codex.responses.transport"
)

// appendBuiltinCodexRetryPolicies appends Clyde's builtin Codex retry
// policies to the operator-supplied set. The builtins live here, in the
// codex provider package, so Codex-specific retry knowledge stays out
// of the generic config layer. An operator policy already present under
// the same name suppresses the matching builtin.
func appendBuiltinCodexRetryPolicies(policies []adapterretry.Policy) []adapterretry.Policy {
	policies = appendCodexRetryPolicyIfAbsent(policies, builtinCodexOverloadedRetryPolicy())
	policies = appendCodexRetryPolicyIfAbsent(policies, builtinCodexTransportRetryPolicy())
	return policies
}

func appendCodexRetryPolicyIfAbsent(policies []adapterretry.Policy, builtin adapterretry.Policy) []adapterretry.Policy {
	for _, policy := range policies {
		if strings.EqualFold(strings.TrimSpace(policy.Name), builtin.Name) {
			return policies
		}
	}
	return append(policies, builtin)
}

// builtinCodexOverloadedRetryPolicy retries the upstream Codex overload
// response. It is gated on the overload message so only that specific
// transient upstream state retries under this policy.
func builtinCodexOverloadedRetryPolicy() adapterretry.Policy {
	return adapterretry.Policy{
		Name:                     codexOverloadedRetryPolicyName,
		Enabled:                  true,
		MaxAttempts:              3,
		InitialDelay:             250 * time.Millisecond,
		MaxDelay:                 2 * time.Second,
		Multiplier:               2,
		JitterFraction:           0.2,
		RetryWhenResponseStarted: false,
		Match: adapterretry.Matchers{
			Backends:          []string{"codex"},
			Operations:        []string{codexWebsocketRetryOperation, codexHTTPRetryOperation},
			Statuses:          nil,
			ErrorClasses:      []string{"scanner_error", "websocket_error", "response_failed"},
			ErrorCodes:        nil,
			MessageSubstrings: []string{"Our servers are currently overloaded. Please try again later."},
		},
	}
}

// builtinCodexTransportRetryPolicy retries Codex transport failures (bad
// handshake, EOF, dropped connection) that surface before the response
// stream starts. It carries no message-substring gate, so
// connection-level errors retry. The overload policy only matches the
// upstream overload message, which left transport failures unretried.
func builtinCodexTransportRetryPolicy() adapterretry.Policy {
	return adapterretry.Policy{
		Name:                     codexTransportRetryPolicyName,
		Enabled:                  true,
		MaxAttempts:              3,
		InitialDelay:             250 * time.Millisecond,
		MaxDelay:                 2 * time.Second,
		Multiplier:               2,
		JitterFraction:           0.2,
		RetryWhenResponseStarted: false,
		Match: adapterretry.Matchers{
			Backends:          []string{"codex"},
			Operations:        []string{codexWebsocketRetryOperation, codexHTTPRetryOperation},
			Statuses:          nil,
			ErrorClasses:      []string{"scanner_error", "websocket_error", "response_failed"},
			ErrorCodes:        nil,
			MessageSubstrings: nil,
		},
	}
}
