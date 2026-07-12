package tokencount

import "math"

// heuristicCounter estimates tokens as a fixed ratio of byte length. It is the
// dependency-free fallback when no tokenizer applies.
type heuristicCounter struct {
	charsPerToken float64
}

// Estimate returns the ceiling of byte length divided by the chars-per-token
// ratio, so the estimate never rounds a non-empty string down to zero.
func (h heuristicCounter) Estimate(text string) int {
	ratio := h.charsPerToken
	if ratio <= 0 {
		ratio = DefaultCharsPerToken
	}
	return int(math.Ceil(float64(len(text)) / ratio))
}
