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

// RedactPolicy is the worker-internal mirror of the
// [autoname.redact] config block PR3 added to internal/config. The
// worker takes the boundary cast at the daemon entry so the
// regex-driven scrubbers below stay decoupled from the toml shape.
//
// Keeping the struct local also lets tests construct a policy
// without importing the config package.
type RedactPolicy struct {
	// Numbers controls the long-digit scrubber. When true, runs of
	// 7 or more consecutive digits collapse to "<num>".
	Numbers bool
	// Emails controls the email-shape scrubber. When true, tokens
	// shaped like "local@host" collapse to "<email>".
	Emails bool
	// Paths controls the absolute-path scrubber. When true, tokens
	// that start with a slash followed by path-shaped characters
	// collapse to "<path>".
	Paths bool
	// Keys controls the API key prefix scrubber. When true, tokens
	// that start with a known provider prefix and carry an alnum
	// tail collapse to "<key>".
	Keys bool
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
	limit := min(3, len(messages))
	parts := make([]string, 0, limit)
	for index, message := range messages {
		if index >= limit {
			break
		}
		parts = append(parts, Redact(message, policy))
	}
	joined := strings.Join(parts, MessageSeparator)
	if len(joined) > JoinedCharCap {
		joined = joined[:JoinedCharCap]
	}
	return joined
}

// redactSourceText is the per-line redaction PR4's source extractor
// calls before fingerprinting the candidate. It runs the same
// scrubbers as Redact but with all toggles forced on; the worker
// only consults RedactPolicy at the LLM boundary, not at the
// transcript-read boundary.
func redactSourceText(text string) string {
	return Redact(strings.TrimSpace(text), DefaultRedactPolicy())
}
