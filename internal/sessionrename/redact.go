package sessionrename

import (
	"regexp"
	"strings"
)

// redactSourceText strips content classes we do not want the naming path to see.
func redactSourceText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = redactEmails.ReplaceAllString(s, " ")
	s = redactPaths.ReplaceAllString(s, " ")
	s = redactKeyish.ReplaceAllString(s, " ")
	s = redactLongDigits.ReplaceAllString(s, " ")
	s = redactCollapseWhitespace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

var (
	redactEmails             = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	redactPaths              = regexp.MustCompile(`(?:^|\s)(?:/|~/|\./)[A-Za-z0-9._\-/]+`)
	redactKeyish             = regexp.MustCompile(`(?i)\b(?:sk|pk|key|token|secret)[-_][A-Za-z0-9]{8,}`)
	redactLongDigits         = regexp.MustCompile(`\d{7,}`)
	redactCollapseWhitespace = regexp.MustCompile(`\s+`)
)
