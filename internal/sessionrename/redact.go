// Package sessionrename owns the auto-rename worker, the redaction
// pipeline, the candidate validation, and the LLM-backed candidate
// source.
//
// PR5 contribution: this file implements the redaction pass that
// scrubs the first three user messages before they reach the
// configured candidate source. The redaction toggles match the
// RedactPolicy struct PR3 added under [autoname.redact] in the
// daemon config.
package sessionrename

import (
	"regexp"
	"strings"
)

// MessageCharCap is the per-message character ceiling. The plan caps
// each of the first three user messages at 240 characters before
// redaction so the worker never reads runaway prose.
const MessageCharCap = 240

// JoinedCharCap is the ceiling on the concatenated redacted output.
// Section 8 of the plan caps the LLM-visible text at 800 characters
// so prompt size stays bounded across providers.
const JoinedCharCap = 800

// MessageSeparator joins the three redacted messages. The separator
// is a literal "\n---\n" so the LLM sees clear boundaries between
// turns without needing to parse JSON.
const MessageSeparator = "\n---\n"

// RedactPolicy mirrors the [autoname.redact] config block PR3 added
// to internal/config. The struct is duplicated here as a placeholder
// so PR5's package compiles ahead of PR3's merge. Once PR3 lands,
// this struct should move to internal/config and this package will
// import it.
type RedactPolicy struct {
	// Numbers controls the long-digit scrubber. When true, runs of
	// 7 or more consecutive digits collapse to "<num>".
	Numbers bool `toml:"numbers"`
	// Emails controls the email-shape scrubber. When true, tokens
	// shaped like "local@host" collapse to "<email>".
	Emails bool `toml:"emails"`
	// Paths controls the absolute-path scrubber. When true, tokens
	// that start with a slash followed by path-shaped characters
	// collapse to "<path>".
	Paths bool `toml:"paths"`
	// Keys controls the API key prefix scrubber. When true, tokens
	// that start with a known provider prefix and carry an alnum
	// tail collapse to "<key>".
	Keys bool `toml:"keys"`
}

// DefaultRedactPolicy returns the policy the worker uses when the
// operator has not overridden any toggle. Every scrubber defaults to
// on so the LLM-visible content is conservative by default.
func DefaultRedactPolicy() RedactPolicy {
	return RedactPolicy{
		Numbers: true,
		Emails:  true,
		Paths:   true,
		Keys:    true,
	}
}

// Compiled regexes scoped to this package. Each pattern matches one
// scrubber category. The patterns are anchored on whitespace or word
// boundaries where the substitution should leave surrounding prose
// alone.
var (
	// longDigitsRe matches runs of 7 or more digits. Six-digit and
	// shorter runs pass through so dates, version numbers, and small
	// counts stay readable.
	longDigitsRe = regexp.MustCompile(`\d{7,}`)

	// emailRe matches a token of "local@host" shape. Both halves
	// must be non-empty and contain only typical email characters.
	// The pattern intentionally rejects two-at tokens because the
	// plan only scrubs single-at shapes.
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

	// absPathRe matches tokens that start with a slash and are
	// followed by alnum or path characters. The minimum length is
	// two so a bare "/" does not trigger a substitution.
	absPathRe = regexp.MustCompile(`/[A-Za-z0-9._\-/]+[A-Za-z0-9._\-]`)

	// keyPrefixRe matches the documented provider key prefixes plus
	// an alnum tail. The alternatives are ordered from longest to
	// shortest so the regex engine prefers the most specific match.
	keyPrefixRe = regexp.MustCompile(`(?:sk-|ghp_|gho_|AKIA|AIza|xoxb-|xoxp-|xoxc-)[A-Za-z0-9_\-]{8,}`)
)

// Redact applies the configured redaction toggles to one message and
// returns the scrubbed text. The function caps the input at
// MessageCharCap before scrubbing so a runaway prompt cannot exhaust
// the regex engine on a single call.
func Redact(message string, policy RedactPolicy) string {
	trimmed := message
	if len(trimmed) > MessageCharCap {
		trimmed = trimmed[:MessageCharCap]
	}
	if policy.Keys {
		trimmed = keyPrefixRe.ReplaceAllString(trimmed, "<key>")
	}
	if policy.Emails {
		trimmed = emailRe.ReplaceAllString(trimmed, "<email>")
	}
	if policy.Paths {
		trimmed = absPathRe.ReplaceAllString(trimmed, "<path>")
	}
	if policy.Numbers {
		trimmed = longDigitsRe.ReplaceAllString(trimmed, "<num>")
	}
	return trimmed
}

// RedactMessages applies Redact to up to the first three user
// messages, joins them with MessageSeparator, and caps the joined
// result at JoinedCharCap. The function tolerates fewer than three
// messages: the join still happens so the worker can pass the
// partial output downstream.
func RedactMessages(messages []string, policy RedactPolicy) string {
	limit := 3
	if len(messages) < limit {
		limit = len(messages)
	}
	parts := make([]string, 0, limit)
	for index := 0; index < limit; index++ {
		parts = append(parts, Redact(messages[index], policy))
	}
	joined := strings.Join(parts, MessageSeparator)
	if len(joined) > JoinedCharCap {
		joined = joined[:JoinedCharCap]
	}
	return joined
}
