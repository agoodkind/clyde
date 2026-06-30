// Package wireflags strips operator-named tokens from a provider's outbound
// capability header (anthropic-beta, x-codex-beta-features, openai-beta), which
// all carry a comma-joined token list. The tokens to drop are provider-neutral
// operator config, not compiled-in vendor strings, so the codebase ships none
// of any provider's proprietary capability vocabulary.
package wireflags

import "strings"

// Suppress removes every token in drop from the comma-joined value, preserving
// the order of the surviving tokens. Matching is exact per trimmed token and
// case-insensitive. It returns the rebuilt value and the list of removed tokens
// in the order they appeared in value, so the caller can log exactly what
// changed. A token in drop that is absent from value is silently ignored, and
// an empty value or empty drop list returns the value unchanged.
func Suppress(value string, drop []string) (kept string, removed []string) {
	if strings.TrimSpace(value) == "" || len(drop) == 0 {
		return value, nil
	}
	dropSet := make(map[string]bool, len(drop))
	for _, token := range drop {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" {
			continue
		}
		dropSet[strings.ToLower(trimmed)] = true
	}
	if len(dropSet) == 0 {
		return value, nil
	}
	keptTokens := make([]string, 0)
	removedTokens := make([]string, 0)
	for raw := range strings.SplitSeq(value, ",") {
		token := strings.TrimSpace(raw)
		if token == "" {
			continue
		}
		if dropSet[strings.ToLower(token)] {
			removedTokens = append(removedTokens, token)
			continue
		}
		keptTokens = append(keptTokens, token)
	}
	if len(removedTokens) == 0 {
		return value, nil
	}
	return strings.Join(keptTokens, ","), removedTokens
}
