package sessionrename

import (
	"regexp"
	"strings"

	"goodkind.io/clyde/internal/session"
)

var uuidLikeRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

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

type candidateName struct {
	Name   string
	Reason string
}

type candidateRejectedError struct{ reason string }

func (e *candidateRejectedError) Error() string {
	return "sessionrename: candidate rejected: " + e.reason
}

func rejected(reason string) error { return &candidateRejectedError{reason: reason} }

const maxNameWords = 6

func propose(src candidateSource, taken map[string]bool) (candidateName, error) {
	if src.Text == "" {
		return newCandidateName("", "empty_source"), rejected("empty_source")
	}
	sanitized := session.SanitizeLegacySlugName(condense(src))
	if sanitized == "" {
		return newCandidateName("", "sanitize_empty"), rejected("sanitize_empty")
	}
	if uuidLikeRegex.MatchString(sanitized) {
		return newCandidateName("", "uuid_like"), rejected("uuid_like")
	}
	candidate := session.UniqueLegacySlugName(sanitized, taken)
	if candidate == sanitized && taken[sanitized] {
		return newCandidateName("", "collision"), rejected("collision")
	}
	if err := session.ValidateLegacySlugName(candidate); err != nil {
		return newCandidateName("", "validate_failed"), rejected("validate_failed")
	}
	return newCandidateName(candidate, ""), nil
}

func condense(src candidateSource) string {
	switch src.Kind {
	case sourceCustomTitle:
		return src.Text
	case sourceFirstUserMessage:
		return condenseUserMessage(src.Text)
	default:
		return src.Text
	}
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
		if len(keep) == maxNameWords {
			break
		}
	}
	if len(keep) == 0 {
		return text
	}
	return strings.Join(keep, "-")
}

func newCandidateName(name, reason string) candidateName {
	return candidateName{
		Name:   name,
		Reason: reason,
	}
}
