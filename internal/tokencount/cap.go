package tokencount

import "strings"

// CapToLastTokens keeps the last whole lines of body whose summed per-line token
// estimate stays at or under budget. It scans backward from the end and cuts on
// a line boundary, so the result is valid UTF-8 and ends with a newline when it
// was truncated. A budget at or below zero, or a body already within budget,
// returns body unchanged with truncated false.
//
// Per-line summation is intentionally conservative: the summed estimate is at
// least the whole-suffix estimate, so a result under budget guarantees the real
// token count is under budget. When even the final single line exceeds budget,
// the result is the empty string.
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
