package retry

import (
	"context"
	"log/slog"
	"math"
	"math/rand"
	"strings"
	"time"

	"goodkind.io/clyde/internal/config"
)

// Policy is one adapter retry rule compiled from config.
type Policy struct {
	Name                     string
	Enabled                  bool
	MaxAttempts              int
	InitialDelay             time.Duration
	MaxDelay                 time.Duration
	Multiplier               float64
	JitterFraction           float64
	RetryWhenResponseStarted bool
	Match                    Matchers
}

// Matchers constrains when a policy applies. Empty matcher slices mean
// unconstrained for that field.
type Matchers struct {
	Backends          []string
	Operations        []string
	Statuses          []int
	ErrorClasses      []string
	ErrorCodes        []string
	MessageSubstrings []string
}

// Signal describes the terminal state from one operation attempt.
type Signal struct {
	Backend         string
	Operation       string
	Status          int
	ErrorClass      string
	ErrorCode       string
	Message         string
	ResponseStarted bool
}

// AttemptLogContext carries request metadata for retry decision logs.
type AttemptLogContext struct {
	RequestID string
	TraceID   string
	ChatKey   string
	Operation string
}

// Decision is the outcome of evaluating policies for a failed attempt.
type Decision struct {
	Retry      bool
	PolicyName string
	Delay      time.Duration
	Reason     string
}

// Sleeper waits before a retry. Tests can replace it with a recorder.
type Sleeper func(context.Context, time.Duration) error

// RandFloat returns a float in [0, 1). Tests can inject deterministic values.
type RandFloat func() float64

// FromConfig compiles adapter retry config into package-local policies.
func FromConfig(cfg config.AdapterRetry) []Policy {
	policies := make([]Policy, 0, len(cfg.Policies))
	for _, item := range cfg.Policies {
		enabled := true
		if item.Enabled != nil {
			enabled = *item.Enabled
		}
		policies = append(policies, Policy{
			Name:                     strings.TrimSpace(item.Name),
			Enabled:                  enabled,
			MaxAttempts:              item.MaxAttempts,
			InitialDelay:             item.InitialDelay.Duration(),
			MaxDelay:                 item.MaxDelay.Duration(),
			Multiplier:               item.Multiplier,
			JitterFraction:           item.JitterFraction,
			RetryWhenResponseStarted: item.RetryWhenResponseStarted,
			Match: Matchers{
				Backends:          cloneStrings(item.Match.Backends),
				Operations:        cloneStrings(item.Match.Operations),
				Statuses:          cloneInts(item.Match.Statuses),
				ErrorClasses:      cloneStrings(item.Match.ErrorClasses),
				ErrorCodes:        cloneStrings(item.Match.ErrorCodes),
				MessageSubstrings: cloneStrings(item.Match.MessageSubstrings),
			},
		})
	}
	return policies
}

// Decide returns the first matching retry decision for attempt. Attempt is
// one-based and includes the original attempt.
func Decide(policies []Policy, signal Signal, attempt int, random RandFloat) Decision {
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		if !policy.Matches(signal) {
			continue
		}
		if signal.ResponseStarted && !policy.RetryWhenResponseStarted {
			return Decision{PolicyName: policy.Name, Reason: "response_started"}
		}
		if attempt >= policy.MaxAttempts {
			return Decision{PolicyName: policy.Name, Reason: "max_attempts_exhausted"}
		}
		return Decision{
			Retry:      true,
			PolicyName: policy.Name,
			Delay:      policy.DelayForAttempt(attempt, random),
			Reason:     "matched",
		}
	}
	return Decision{Reason: "no_policy_matched"}
}

// Matches reports whether a policy applies to a failed operation.
func (p Policy) Matches(signal Signal) bool {
	if !matchStringSet(p.Match.Backends, signal.Backend) {
		return false
	}
	if !matchStringSet(p.Match.Operations, signal.Operation) {
		return false
	}
	if !matchIntSet(p.Match.Statuses, signal.Status) {
		return false
	}
	if !matchStringSet(p.Match.ErrorClasses, signal.ErrorClass) {
		return false
	}
	if !matchStringSet(p.Match.ErrorCodes, signal.ErrorCode) {
		return false
	}
	return matchMessageSubstrings(p.Match.MessageSubstrings, signal.Message)
}

// DelayForAttempt returns the bounded backoff for the attempt that just failed.
func (p Policy) DelayForAttempt(attempt int, random RandFloat) time.Duration {
	delay := p.InitialDelay
	if delay < 0 {
		delay = 0
	}
	multiplier := p.Multiplier
	if multiplier == 0 {
		multiplier = 1
	}
	if attempt > 1 && delay > 0 {
		power := math.Pow(multiplier, float64(attempt-1))
		delay = time.Duration(float64(delay) * power)
	}
	if p.MaxDelay > 0 && delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	if delay <= 0 || p.JitterFraction <= 0 {
		return delay
	}
	if random == nil {
		random = rand.Float64
	}
	jitterRange := float64(delay) * p.JitterFraction
	offset := (random()*2 - 1) * jitterRange
	jittered := time.Duration(float64(delay) + offset)
	if jittered < 0 {
		return 0
	}
	if p.MaxDelay > 0 && jittered > p.MaxDelay {
		return p.MaxDelay
	}
	return jittered
}

// Sleep waits for the provided delay or context cancellation.
func Sleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// LogDecision emits the common retry-decision shape.
func LogDecision(ctx context.Context, log *slog.Logger, decision Decision, attempt int, maxAttempts int, info AttemptLogContext, finalOutcome string) {
	if log == nil {
		log = slog.Default()
	}
	log.InfoContext(ctx, "adapter.retry.decision",
		"component", "adapter",
		"subcomponent", "retry",
		"retry_policy", decision.PolicyName,
		"attempt", attempt,
		"max_attempts", maxAttempts,
		"delay_ms", decision.Delay.Milliseconds(),
		"request_id", info.RequestID,
		"trace_id", info.TraceID,
		"chat_key", info.ChatKey,
		"operation", info.Operation,
		"final_outcome", finalOutcome,
		"reason", decision.Reason,
	)
}

func MaxAttempts(policies []Policy, policyName string) int {
	for _, policy := range policies {
		if policy.Name == policyName {
			return policy.MaxAttempts
		}
	}
	return 0
}

func matchStringSet(values []string, got string) bool {
	if len(values) == 0 {
		return true
	}
	normalized := strings.TrimSpace(got)
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), normalized) {
			return true
		}
	}
	return false
}

func matchIntSet(values []int, got int) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if value == got {
			return true
		}
	}
	return false
}

func matchMessageSubstrings(values []string, got string) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if strings.Contains(got, value) {
			return true
		}
	}
	return false
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func cloneInts(values []int) []int {
	if len(values) == 0 {
		return nil
	}
	out := make([]int, len(values))
	copy(out, values)
	return out
}
