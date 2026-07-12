package util

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Suffix multipliers for human-friendly counts.
const (
	humanThousand = 1_000
	humanMillion  = 1_000_000
	// wholeNumberEpsilon bounds the rounding tolerance when a decimal mantissa
	// combines with a suffix, so "1.5k" is accepted and "0.0001k" is rejected.
	wholeNumberEpsilon = 1e-9
)

// ParseHumanCount parses a human-friendly token-size string into a count.
//
// It accepts a plain integer, comma grouping, an optional decimal mantissa, and
// an optional case-insensitive k (thousand) or m (million) suffix, so "200000",
// "200,000", "200k", and "1.5m" all parse. An empty string returns 0, which
// means uncapped. The result must be a non-negative whole number that fits in
// int; a negative value, non-numeric text, a bare suffix, or a fractional
// result returns an error naming the input.
func ParseHumanCount(s string) (int, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, nil
	}

	cleaned := strings.ReplaceAll(trimmed, ",", "")
	if cleaned == "" {
		return 0, fmt.Errorf("parse token count %q: no digits", s)
	}

	multiplier := 1.0
	switch cleaned[len(cleaned)-1] {
	case 'k', 'K':
		multiplier = humanThousand
		cleaned = cleaned[:len(cleaned)-1]
	case 'm', 'M':
		multiplier = humanMillion
		cleaned = cleaned[:len(cleaned)-1]
	}
	if cleaned == "" {
		return 0, fmt.Errorf("parse token count %q: missing number before suffix", s)
	}

	value, err := strconv.ParseFloat(cleaned, 64)
	if err != nil {
		return 0, fmt.Errorf("parse token count %q: not a number", s)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("parse token count %q: not a finite number", s)
	}
	if value < 0 {
		return 0, fmt.Errorf("parse token count %q: must not be negative", s)
	}

	scaled := value * multiplier
	if scaled > float64(math.MaxInt) {
		return 0, fmt.Errorf("parse token count %q: exceeds maximum", s)
	}
	rounded := math.Round(scaled)
	if math.Abs(scaled-rounded) > wholeNumberEpsilon {
		return 0, fmt.Errorf("parse token count %q: not a whole number of tokens", s)
	}
	return int(rounded), nil
}
