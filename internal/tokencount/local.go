package tokencount

import "math"

// scaledCounter multiplies another counter's estimate by a factor and rounds up,
// so a proxy tokenizer can be made conservative for a heavier target.
type scaledCounter struct {
	base   Counter
	factor float64
}

// Estimate returns the ceiling of the base estimate times the factor.
func (s scaledCounter) Estimate(text string) int {
	factor := s.factor
	if factor <= 0 {
		factor = 1
	}
	return int(math.Ceil(float64(s.base.Estimate(text)) * factor))
}

// LocalCounter returns the best local token estimator for a provider family.
// FamilyClaude uses the o200k proxy padded by the safety factor. FamilyGPT uses
// the exact o200k count. FamilyUnknown infers a family from the model, then
// falls back to the chars-per-token heuristic.
func LocalCounter(family Family, model string, s Settings) Counter {
	heuristic := heuristicCounter{charsPerToken: s.charsPerToken()}
	tik := tiktokenCounter{fallback: heuristic}
	switch family {
	case FamilyClaude:
		return scaledCounter{base: tik, factor: s.safetyFactor()}
	case FamilyGPT:
		return tik
	case FamilyUnknown:
		inferred := FamilyFromModel(model)
		if inferred == FamilyUnknown {
			return heuristic
		}
		return LocalCounter(inferred, model, s)
	default:
		return heuristic
	}
}
