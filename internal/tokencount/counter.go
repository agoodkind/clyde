// Package tokencount estimates the number of model tokens in text and caps
// rendered output to a token budget. It selects a local estimator by provider
// family and falls back to a chars-per-token heuristic when no tokenizer fits.
package tokencount

import "strings"

// Default estimator tuning. SafetyFactor pads Claude estimates because current
// Claude models tokenize heavier than the o200k tokenizer used as a proxy.
const (
	// DefaultSafetyFactor pads the o200k proxy count for Claude targets.
	DefaultSafetyFactor = 1.3
	// DefaultCharsPerToken is the heuristic ratio of bytes per token.
	DefaultCharsPerToken = 3.5
)

// Counter estimates the number of tokens in text. Implementations are pure and
// must not perform network I/O.
type Counter interface {
	Estimate(text string) int
}

// Family is the tokenizer family used to count a transcript.
type Family int

const (
	// FamilyUnknown means the provider did not map to a known tokenizer.
	FamilyUnknown Family = iota
	// FamilyClaude counts with the Claude proxy (o200k plus a safety factor).
	FamilyClaude
	// FamilyGPT counts with the exact o200k tokenizer.
	FamilyGPT
)

// FamilyFromModel infers a tokenizer family from a model identifier. It is a
// fallback for providers that do not map directly to a family.
func FamilyFromModel(model string) Family {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "claude"):
		return FamilyClaude
	case strings.Contains(lower, "gpt"),
		strings.Contains(lower, "codex"),
		strings.Contains(lower, "o1"),
		strings.Contains(lower, "o3"),
		strings.Contains(lower, "o4"):
		return FamilyGPT
	default:
		return FamilyUnknown
	}
}

// Settings tunes the local estimators. Zero values fall back to the package
// defaults so a zero Settings is usable.
type Settings struct {
	SafetyFactor  float64
	CharsPerToken float64
}

func (s Settings) safetyFactor() float64 {
	if s.SafetyFactor > 0 {
		return s.SafetyFactor
	}
	return DefaultSafetyFactor
}

func (s Settings) charsPerToken() float64 {
	if s.CharsPerToken > 0 {
		return s.CharsPerToken
	}
	return DefaultCharsPerToken
}
