package sessionrename

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"goodkind.io/clyde/internal/session"
)

// uuidLikeRegex matches the UUIDv4 shape Claude uses for provider
// session ids. The validator rejects candidate names that look like
// a provider session id so an accidental id-shaped first user
// message never becomes a directory name.
var uuidLikeRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// kebabRegex is the strict kebab-case shape the worker requires for
// every candidate. It is tighter than session.ValidateName because
// auto-generated names should never start with a digit and should
// never run shorter than three characters.
var kebabRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{1,48}[a-z0-9]$`)

// stopwords is the trimmed list of common English filler words the
// namer drops when condensing a first user message into a label.
// The list is intentionally short; we want enough signal to keep
// distinctive nouns and verbs while losing leading "please can you".
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true,
	"please": true, "can": true, "could": true, "would": true,
	"you": true, "we": true, "i": true,
	"to": true, "of": true, "for": true, "on": true, "in": true,
	"and": true, "or": true, "but": true, "with": true,
	"my": true, "is": true, "are": true, "was": true, "were": true,
	"this": true, "that": true, "these": true, "those": true,
	"it": true, "its": true,
	"do": true, "does": true, "did": true, "be": true, "been": true,
	"have": true, "has": true, "had": true,
	"will": true, "should": true, "shall": true,
}

// MaxNameWords caps the rendered candidate at a sensible number of
// words so a runaway first user message does not produce a 64-char
// name.
const MaxNameWords = 6

// CandidateName is the namer's output. Reason explains a non-nil
// error in machine-readable form so structured logs can attribute
// the rejection cleanly.
type CandidateName struct {
	Name   string
	Reason string
}

// errCandidateRejected is the umbrella error the namer returns when
// a proposed name fails validation. The Reason string on
// CandidateName carries the specific rejection cause.
type errCandidateRejected struct{ reason string }

func (e *errCandidateRejected) Error() string {
	return fmt.Sprintf("sessionrename: candidate rejected: %s", e.reason)
}

// rejected is shorthand for returning a typed rejection from the
// validator.
func rejected(reason string) error { return &errCandidateRejected{reason: reason} }

// IsRejected reports whether err originates from candidate
// validation. Callers can use this to attribute rejected candidates
// without importing the unexported error type.
func IsRejected(err error) bool {
	var rej *errCandidateRejected
	return errors.As(err, &rej)
}

// Propose converts a CandidateSource into a candidate session name.
// It runs Sanitize to map arbitrary text into the directory charset,
// trims to MaxNameWords, validates the result against the existing
// taken set, and asks UniqueName to suffix-increment once on
// collision. A failure to produce a valid name returns an
// errCandidateRejected with a structured reason.
func Propose(src CandidateSource, taken map[string]bool) (CandidateName, error) {
	if src.Text == "" {
		return CandidateName{Reason: "empty_source"}, rejected("empty_source")
	}
	sanitized := session.Sanitize(condense(src))
	if sanitized == "" {
		return CandidateName{Reason: "sanitize_empty"}, rejected("sanitize_empty")
	}
	if uuidLikeRegex.MatchString(sanitized) {
		return CandidateName{Reason: "uuid_like"}, rejected("uuid_like")
	}
	if looksLikePath(src.Text) {
		return CandidateName{Reason: "path_like"}, rejected("path_like")
	}
	if !kebabRegex.MatchString(sanitized) {
		return CandidateName{Reason: "kebab_invalid"}, rejected("kebab_invalid")
	}
	candidate := session.UniqueName(sanitized, taken)
	if candidate == sanitized && taken[sanitized] {
		return CandidateName{Reason: "collision"}, rejected("collision")
	}
	if err := session.ValidateName(candidate); err != nil {
		return CandidateName{Reason: "validate_failed"}, rejected("validate_failed")
	}
	return CandidateName{Name: candidate}, nil
}

// condense reduces source text to a short list of distinctive words.
// The first user message is the only kind PR4 emits, so we drop
// stopwords first and keep at most MaxNameWords leading words.
func condense(src CandidateSource) string {
	if src.Kind != SourceFirstUserMessage {
		return src.Text
	}
	return condenseUserMessage(src.Text)
}

func condenseUserMessage(text string) string {
	fields := strings.Fields(strings.ToLower(text))
	keep := make([]string, 0, len(fields))
	for _, word := range fields {
		clean := strings.Trim(word, ".,!?:;\"'()[]{}")
		if clean == "" {
			continue
		}
		if stopwords[clean] {
			continue
		}
		keep = append(keep, clean)
		if len(keep) == MaxNameWords {
			break
		}
	}
	if len(keep) == 0 {
		// Stopword-only input: fall back to the original head so
		// Sanitize still has something to work with.
		return text
	}
	return strings.Join(keep, "-")
}

// looksLikePath reports whether the raw source text looks like a
// file path the worker should reject before sanitization can strip
// the slashes. The check is intentionally conservative; only
// canonical path prefixes count.
func looksLikePath(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "/") {
		return true
	}
	if strings.HasPrefix(trimmed, "~/") {
		return true
	}
	if strings.HasPrefix(trimmed, "./") {
		return true
	}
	return false
}
