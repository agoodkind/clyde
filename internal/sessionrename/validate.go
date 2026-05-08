package sessionrename

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Validation regexes. The kebab-case rule matches the plan's Section
// 8 spec: 4 to 48 characters, lowercase, starts with a letter, ends
// with alnum, hyphens permitted in the middle. The UUID rule
// matches the canonical UUIDv4 shape so the worker can reject names
// that look like provider session ids.
var (
	// kebabRe enforces ^[a-z][a-z0-9-]{2,46}[a-z0-9]$. The middle
	// length is 2 to 46 characters so the total length lands in
	// the 4 to 48 character window the plan specifies.
	kebabRe = regexp.MustCompile(`^[a-z][a-z0-9-]{2,46}[a-z0-9]$`)

	// uuidRe matches the canonical UUIDv4 shape with hyphens. The
	// worker rejects any candidate that matches because that shape
	// indicates the LLM echoed a provider session id.
	uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// Validation error sentinels. Callers can match against these so the
// worker can emit specific slog reasons without parsing the error
// string.
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
// the name is user-owned. The worker passes this map into Validate
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
	// Candidate is the LLM-returned or transcript-derived name.
	Candidate string
	// ExistingNames lists every other session's name with the
	// owner attribution. The validator uses this list for both
	// the user-owned reject rule and the suffix increment rule.
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
func Validate(input ValidationInput) (string, error) {
	candidate := strings.TrimSpace(input.Candidate)
	if candidate == "" {
		return "", ErrCandidateEmpty
	}
	if strings.Contains(candidate, "/") {
		return "", ErrCandidatePath
	}
	if !kebabRe.MatchString(candidate) {
		return "", fmt.Errorf("%w: %q", ErrCandidateShape, candidate)
	}
	if uuidRe.MatchString(candidate) {
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
	// Collision on a non-user-owned name. Try -2 through -9.
	for suffix := 2; suffix <= 9; suffix++ {
		next := fmt.Sprintf("%s-%d", candidate, suffix)
		if !kebabRe.MatchString(next) {
			continue
		}
		switch owners[next] {
		case ownerNone:
			return next, nil
		case ownerUser:
			return "", ErrCandidateUserOwned
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
