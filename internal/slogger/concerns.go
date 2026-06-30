package slogger

import (
	"strings"
)

// ConcernRelPath returns the relative JSONL path under the concern root for the
// named concern. Callers join the result with [DefaultConcernRoot] to obtain
// the absolute on-disk path. The mapping is the dotted concern name with dots
// replaced by path separators and a ".jsonl" suffix (so "adapter.http.ingress"
// maps to "adapter/http/ingress.jsonl"). An empty or whitespace-only concern
// returns the empty string. This matches the path layout the gklog router uses
// when slogger installs it as the per-concern file router.
func ConcernRelPath(concern string) string {
	c := strings.TrimSpace(concern)
	if c == "" {
		return ""
	}
	return strings.ReplaceAll(c, ".", "/") + ".jsonl"
}
