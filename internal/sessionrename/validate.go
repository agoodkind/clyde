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
var uuidLikeRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// kebabRegex is the strict kebab-case shape the worker requires for
// every candidate. It is tighter than session.ValidateName because
// auto-generated names should never start with a digit and should
// never run shorter than three characters. The middle length is 2 to
// 46 characters so the total length lands in the 4 to 48 character
// window the plan specifies.
var kebabRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{2,46}[a-z0-9]$`)

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

// candidateRejectedError is the umbrella error the namer returns when
// a proposed name fails validation. The Reason string on
// CandidateName carries the specific rejection cause.
type candidateRejectedError struct{ reason string }

func (e *candidateRejectedError) Error() string {
	return "sessionrename: candidate rejected: " + e.reason
}

// rejected is shorthand for returning a typed rejection from the
// validator.
func rejected(reason string) error { return &candidateRejectedError{reason: reason} }

// IsRejected reports whether err originates from candidate
// validation. Callers can use this to attribute rejected candidates
// without importing the unexported error type.
func IsRejected(err error) bool {
	var rej *candidateRejectedError
	return errors.As(err, &rej)
}

// Propose converts a CandidateSource into a candidate session name.
// It runs Sanitize to map arbitrary text into the directory charset,
// trims to MaxNameWords, validates the result against the existing
// taken set, and asks UniqueName to suffix-increment once on
// collision. A failure to produce a valid name returns an
// candidateRejectedError with a structured reason. PR4's worker calls
// Propose; the LLM-source path PR5 introduces routes through
// Validate below for stricter rejection semantics.
func Propose(src CandidateSource, taken map[string]bool) (CandidateName, error) {
	if src.Text == "" {
		return CandidateName{Name: "", Reason: "empty_source"}, rejected("empty_source")
	}
	sanitized := session.Sanitize(condense(src))
	if sanitized == "" {
		return CandidateName{Name: "", Reason: "sanitize_empty"}, rejected("sanitize_empty")
	}
	if uuidLikeRegex.MatchString(sanitized) {
		return CandidateName{Name: "", Reason: "uuid_like"}, rejected("uuid_like")
	}
	if looksLikePath(src.Text) {
		return CandidateName{Name: "", Reason: "path_like"}, rejected("path_like")
	}
	if !kebabRegex.MatchString(sanitized) {
		return CandidateName{Name: "", Reason: "kebab_invalid"}, rejected("kebab_invalid")
	}
	candidate := session.UniqueName(sanitized, taken)
	if candidate == sanitized && taken[sanitized] {
		return CandidateName{Name: "", Reason: "collision"}, rejected("collision")
	}
	if err := session.ValidateName(candidate); err != nil {
		return CandidateName{Name: "", Reason: "validate_failed"}, rejected("validate_failed")
	}
	return CandidateName{Name: candidate, Reason: ""}, nil
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

// Validation error sentinels. PR5's LLM source path matches against
// these so the worker can emit specific slog reasons without parsing
// the error string. Propose returns candidateRejectedError with the
// string Reason; Validate returns one of the sentinels here.
var (
	// ErrCandidateEmpty fires when the LLM returned an empty
	// string after trimming.
	ErrCandidateEmpty = errors.New("sessionrename: candidate is empty")
	// ErrCandidateShape fires when the candidate fails the
	// kebab-case regex.
	ErrCandidateShape = errors.New("sessionrename: candidate fails kebab-case shape")
	// ErrCandidateUUID fires when the candidate matches the
	// UUIDv4 shape.
	ErrCandidateUUID = errors.New("sessionrename: candidate looks like a UUID")
	// ErrCandidatePath fires when the candidate contains a slash.
	ErrCandidatePath = errors.New("sessionrename: candidate contains a path separator")
	// ErrCandidateUserOwned fires when the candidate matches an
	// existing session whose AutoNameSource is user.
	ErrCandidateUserOwned = errors.New("sessionrename: candidate matches a user-owned name")
	// ErrCandidateProviderID fires when the candidate matches the
	// session's provider session id literal.
	ErrCandidateProviderID = errors.New("sessionrename: candidate matches the provider session id")
	// ErrCandidateExhausted fires when the suffix increment ran
	// from -2 through -9 without finding a free name.
	ErrCandidateExhausted = errors.New("sessionrename: candidate suffix exhausted")
)

// ExistingNameOwner records, for one existing session name, whether
// the name is user-owned. The worker passes this slice into Validate
// so the validation path can reject candidates that target a
// user-owned name on another session.
type ExistingNameOwner struct {
	// Name is the existing session.Name value.
	Name string
	// UserOwned is true when the existing session's
	// AutoNameSource equals AutoNameSourceUser.
	UserOwned bool
}

// ValidationInput bundles every piece of context the validator needs
// to decide whether a candidate is acceptable. The struct keeps the
// signature stable as new rules land in later PRs.
type ValidationInput struct {
	// Candidate is the LLM-returned name to validate.
	Candidate string
	// ExistingNames lists every other session's name with the
	// owner attribution. The validator uses this list for both the
	// user-owned reject rule and the suffix increment rule.
	ExistingNames []ExistingNameOwner
	// ProviderSessionID is the session's current provider session
	// id literal. The validator rejects a candidate that matches
	// the literal so the worker never overwrites a useful name
	// with a meaningless id.
	ProviderSessionID string
}

// Validate enforces the candidate rules and returns the accepted
// name. The function applies the suffix increment rule when the
// candidate collides with another session that is not user-owned.
// On hard reject, the function returns one of the error sentinels.
// PR5's LLM-source path calls Validate; PR4's transcript path uses
// Propose for backward compatibility with its existing reason
// strings.
func Validate(input ValidationInput) (string, error) {
	candidate := strings.TrimSpace(input.Candidate)
	if candidate == "" {
		return "", ErrCandidateEmpty
	}
	if strings.Contains(candidate, "/") {
		return "", ErrCandidatePath
	}
	if !kebabRegex.MatchString(candidate) {
		return "", ErrCandidateShape
	}
	if uuidLikeRegex.MatchString(candidate) {
		return "", ErrCandidateUUID
	}
	if input.ProviderSessionID != "" && candidate == input.ProviderSessionID {
		return "", ErrCandidateProviderID
	}
	owners := buildOwnerMap(input.ExistingNames)
	if owners[candidate] == ownerUser {
		return "", ErrCandidateUserOwned
	}
	if owners[candidate] == ownerNone {
		return candidate, nil
	}
	for suffix := 2; suffix <= 9; suffix++ {
		next := fmt.Sprintf("%s-%d", candidate, suffix)
		if !kebabRegex.MatchString(next) {
			continue
		}
		switch owners[next] {
		case ownerNone:
			return next, nil
		case ownerUser:
			return "", ErrCandidateUserOwned
		case ownerOther:
			continue
		}
	}
	return "", ErrCandidateExhausted
}

// ownership classifies an existing-name entry for the validator.
type ownership int

const (
	// ownerNone means no other session carries the name.
	ownerNone ownership = iota
	// ownerOther means another session carries the name but did
	// not set AutoNameSource to user. The validator may try a
	// suffix in this case.
	ownerOther
	// ownerUser means another session carries the name and the
	// AutoNameSource is user. The validator hard rejects.
	ownerUser
)

// buildOwnerMap collapses the existing-name list into a name-to-
// ownership lookup. Duplicate entries resolve to the strictest
// owner so a name marked user-owned anywhere wins over a non-user
// entry for the same name.
func buildOwnerMap(entries []ExistingNameOwner) map[string]ownership {
	owners := make(map[string]ownership, len(entries))
	for _, entry := range entries {
		current := owners[entry.Name]
		next := ownerOther
		if entry.UserOwned {
			next = ownerUser
		}
		if next > current {
			owners[entry.Name] = next
		} else if _, present := owners[entry.Name]; !present {
			owners[entry.Name] = next
		}
	}
	return owners
}
