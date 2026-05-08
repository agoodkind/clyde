package sessionrename

import "strings"

// redactSourceText is the redaction stub for PR4. PR5 fills it in
// with email, path, key-prefix, and long-digit redaction before the
// text reaches the LLM. The transcript-only candidate path that
// ships with PR4 only needs whitespace trimming because Sanitize
// already drops every non kebab-case rune downstream.
func redactSourceText(s string) string {
	return strings.TrimSpace(s)
}
