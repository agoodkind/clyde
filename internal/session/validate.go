package session

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MinNameLength is the minimum allowed session name length
	MinNameLength = 2

	// MaxNameLength is the maximum allowed session name length
	MaxNameLength = 64

	// MinDisplayNameLength is the minimum allowed exact display name length.
	MinDisplayNameLength = 1

	// MaxDisplayNameLength is the maximum allowed exact display name length.
	MaxDisplayNameLength = 200
)

var (
	// sessionNameRegex validates session name format:
	// - Must start and end with alphanumeric (lowercase)
	// - Can contain hyphens in the middle
	// - No consecutive hyphens
	sessionNameRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

	// ErrInvalidName is returned when session name validation fails
	ErrInvalidName = errors.New("invalid session name")

	// ErrInvalidDisplayName is returned when exact display-name validation fails.
	ErrInvalidDisplayName = errors.New("invalid display name")
)

// ValidateName checks if a legacy slug session name is valid.
// Returns an error if the name is invalid, with details about why.
func ValidateName(name string) error {
	if len(name) < MinNameLength {
		return fmt.Errorf("%w: name must be at least %d characters", ErrInvalidName, MinNameLength)
	}

	if len(name) > MaxNameLength {
		return fmt.Errorf("%w: name must be at most %d characters", ErrInvalidName, MaxNameLength)
	}

	if !sessionNameRegex.MatchString(name) {
		return fmt.Errorf("%w: name must be lowercase alphanumeric with hyphens, starting and ending with alphanumeric", ErrInvalidName)
	}

	// Check for consecutive hyphens
	if strings.Contains(name, "--") {
		return fmt.Errorf("%w: name cannot contain consecutive hyphens", ErrInvalidName)
	}

	return nil
}

// ValidateDisplayName checks whether name is safe to store as Clyde's exact
// human-visible session name. The policy intentionally does not slugify or
// lowercase: stable storage and parent linkage use ClydeUUID, while this value
// is for display and exact lookup.
func ValidateDisplayName(name string) error {
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("%w: name cannot have leading or trailing whitespace", ErrInvalidDisplayName)
	}

	if utf8.RuneCountInString(name) < MinDisplayNameLength {
		return fmt.Errorf("%w: name must be at least %d visible character", ErrInvalidDisplayName, MinDisplayNameLength)
	}

	if !utf8.ValidString(name) {
		return fmt.Errorf("%w: name must be valid UTF-8", ErrInvalidDisplayName)
	}

	if utf8.RuneCountInString(name) > MaxDisplayNameLength {
		return fmt.Errorf("%w: name must be at most %d characters", ErrInvalidDisplayName, MaxDisplayNameLength)
	}

	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: name cannot contain control characters", ErrInvalidDisplayName)
		}
	}

	return nil
}

// Sanitize converts an arbitrary string into a valid session name or
// returns "" when the input has no usable alphanumeric content. It
// lowercases the input, replaces every non [a-z0-9] rune with a hyphen,
// collapses runs of hyphens, trims edge hyphens, and truncates to
// MaxNameLength. Callers use this to map a provider-owned user-facing title
// into a clyde session Name. A return of "" signals that the caller should
// fall back to another naming strategy.
func Sanitize(raw string) string {
	if raw == "" {
		return ""
	}
	raw = strings.ToLower(raw)
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := collapseHyphens(b.String())
	out = strings.Trim(out, "-")
	if len(out) > MaxNameLength {
		out = strings.TrimRight(out[:MaxNameLength], "-")
	}
	if ValidateName(out) != nil {
		return ""
	}
	return out
}

// collapseHyphens replaces every run of consecutive hyphens with a
// single hyphen. Used by Sanitize to meet the no-consecutive-hyphens
// rule enforced by ValidateName.
func collapseHyphens(s string) string {
	if !strings.Contains(s, "--") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prevHyphen := false
	for _, r := range s {
		if r == '-' {
			if prevHyphen {
				continue
			}
			prevHyphen = true
		} else {
			prevHyphen = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// NameCollisionMax bounds the counter suffix loop used by UniqueName so
// an adversarial taken set cannot spin forever. Well above any realistic
// number of same-base sessions a user would ever create.
const NameCollisionMax = 1000

// UniqueName returns base if it is not in taken, otherwise appends a
// numeric suffix until the result is unique. Returns base unchanged when
// the collision loop is exhausted; callers should treat that as a
// fall-through signal. Truncates the base when a suffix would push the
// combined length over MaxNameLength so the returned name always passes
// ValidateName.
func UniqueName(base string, taken map[string]bool) string {
	if base == "" {
		return ""
	}
	if !taken[base] {
		return base
	}
	for i := 2; i < NameCollisionMax; i++ {
		suffix := fmt.Sprintf("-%d", i)
		candidate := base
		if len(candidate)+len(suffix) > MaxNameLength {
			candidate = strings.TrimRight(candidate[:MaxNameLength-len(suffix)], "-")
		}
		candidate += suffix
		if !taken[candidate] {
			return candidate
		}
	}
	return base
}

// UniqueDisplayName returns base if it is not taken, otherwise appends a
// human-visible numeric suffix. Invalid display names return "" so callers can
// fall back to stable id based naming.
func UniqueDisplayName(base string, taken map[string]bool) string {
	base = strings.TrimSpace(base)
	if ValidateDisplayName(base) != nil {
		return ""
	}
	if !taken[base] {
		return base
	}
	for i := 2; i < NameCollisionMax; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		maxBaseRunes := MaxDisplayNameLength - utf8.RuneCountInString(suffix)
		candidateBase := truncateDisplayName(base, maxBaseRunes)
		if candidateBase == "" {
			return ""
		}
		candidate := candidateBase + suffix
		if !taken[candidate] {
			return candidate
		}
	}
	return base
}

func truncateDisplayName(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	out := make([]rune, 0, maxRunes)
	for _, r := range value {
		if len(out) == maxRunes {
			break
		}
		out = append(out, r)
	}
	return strings.TrimRightFunc(string(out), unicode.IsSpace)
}
