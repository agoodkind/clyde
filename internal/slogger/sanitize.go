// Package slogger configures the structured logging surface used across
// the daemon, TUI, and adapter. This file holds the canonical chat-key
// sanitizer shared by the transcript router and the codex reasoningstore.
package slogger

import "strings"

// SanitizeChatKey is the canonical sanitizer for chat keys used as filesystem
// path components. It replaces any rune outside [A-Za-z0-9_-] with '_'. Empty
// inputs return the empty string. Inputs that sanitize to all underscores
// still return the underscored form; callers decide whether to reject. Path
// separators, dots, and slashes are normalized so a malicious chat_key like
// "../../etc/passwd" cannot escape its intended root directory.
func SanitizeChatKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	hasAccepted := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
			hasAccepted = true
		default:
			b.WriteRune('_')
		}
	}
	if !hasAccepted {
		return ""
	}
	return b.String()
}
