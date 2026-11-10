package tokencount

import (
	"strings"
	"unicode/utf8"
)

// CapToLastTokens keeps a suffix of body within budget using conservative
// per-line token estimates. It scans whole lines backward from the end. When
// the next older line does not fit, it calls lastFittingSuffix to keep the
// longest end part of that line that fits the remaining budget, then stops.
// The cut starts at a UTF-8 rune boundary, and the result is valid UTF-8.
// A nonempty truncated result ends with a newline. If the remaining budget
// cannot fit any part of that line, it keeps only the whole lines already kept.
// It returns an empty string if it cannot keep any text.
// A budget at or below zero, or a body already within budget, returns body
// unchanged with truncated false.
// The sum of the per-line estimates is at least the estimate for the whole
// suffix. This conservative sum guarantees that the real token count stays
// at or under budget.
func CapToLastTokens(body string, budget int, c Counter) (string, int, bool) {
	if budget <= 0 {
		return body, 0, false
	}
	total := c.Estimate(body)
	if total <= budget {
		return body, total, false
	}

	trimmed := strings.TrimRight(body, "\n")
	if trimmed == "" {
		return body, total, false
	}

	end := len(trimmed)
	cut := len(trimmed)
	keptTokens := 0
	for end > 0 {
		newline := strings.LastIndexByte(trimmed[:end], '\n')
		lineStart := newline + 1
		lineTokens := c.Estimate(trimmed[lineStart:end] + "\n")
		if keptTokens+lineTokens > budget {
			suffixStart, suffixTokens := lastFittingSuffix(trimmed[lineStart:end], budget-keptTokens, c)
			if lineStart+suffixStart < end {
				keptTokens += suffixTokens
				cut = lineStart + suffixStart
			}
			break
		}
		keptTokens += lineTokens
		cut = lineStart
		end = newline
	}

	if cut >= len(trimmed) {
		return "", 0, true
	}
	return trimmed[cut:] + "\n", keptTokens, true
}

// lastFittingSuffix finds the longest end part of line that fits budget.
// The input line does not include its newline. The token estimate includes
// a trailing newline. The function uses binary search over byte offsets that
// start a UTF-8 rune to find the smallest start offset with an estimate at or
// under budget. It returns that offset and its token estimate. It returns
// len(line) and 0 if it cannot fit any part of the line.
func lastFittingSuffix(line string, budget int, c Counter) (int, int) {
	fits := func(start int) (int, bool) {
		tokens := c.Estimate(line[start:] + "\n")
		return tokens, tokens <= budget
	}
	low := 0
	high := len(line)
	for low < high {
		middle := low + (high-low)/2
		for middle > low && !utf8.RuneStart(line[middle]) {
			middle--
		}
		if _, ok := fits(middle); ok {
			high = middle
			continue
		}
		low = middle + 1
		for low < high && !utf8.RuneStart(line[low]) {
			low++
		}
	}
	tokens, ok := fits(high)
	if !ok || high == len(line) {
		return len(line), 0
	}
	return high, tokens
}
