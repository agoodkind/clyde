package transcript

import (
	"strings"
	"time"
	"unicode"
)

// SourceTimeOutcome identifies the result of matching source text to a timestamp.
type SourceTimeOutcome int

const (
	// SourceTimeFound indicates that matching text has one nonzero timestamp.
	SourceTimeFound SourceTimeOutcome = iota
	// SourceTimeNone indicates that the index did not find matching text.
	SourceTimeNone
	// SourceTimeAmbiguous indicates that matching text has different timestamps.
	SourceTimeAmbiguous
	// SourceTimeUndated indicates that matching text has a zero timestamp.
	SourceTimeUndated
)

// SourceTimeEntry associates source text and its role with a timestamp.
type SourceTimeEntry struct {
	Role      string
	Text      string
	Timestamp time.Time
}

type sourceText struct {
	text      string
	timestamp time.Time
}

// SourceTimeIndex indexes consecutive source text entries by role.
type SourceTimeIndex struct {
	runs map[string][][]sourceText
}

// NewSourceTimeIndex groups nonempty source text into consecutive role runs.
func NewSourceTimeIndex(entries []SourceTimeEntry) *SourceTimeIndex {
	index := &SourceTimeIndex{runs: map[string][][]sourceText{}}
	currentRole := ""
	current := make([]sourceText, 0)
	flush := func() {
		if currentRole != "" && len(current) > 0 {
			index.runs[currentRole] = append(index.runs[currentRole], current)
		}
		current = make([]sourceText, 0)
	}
	for _, entry := range entries {
		if entry.Role != currentRole {
			flush()
			currentRole = entry.Role
		}
		trimmed := strings.TrimSpace(entry.Text)
		if trimmed == "" {
			continue
		}
		current = append(current, sourceText{text: trimmed, timestamp: entry.Timestamp})
	}
	flush()
	return index
}

// TextTime returns a timestamp when all matching source text has the same nonzero time.
func (index *SourceTimeIndex) TextTime(role string, text string) (time.Time, SourceTimeOutcome) {
	candidates := index.TextCandidates(role, text)
	if len(candidates) == 0 {
		return time.Time{}, SourceTimeNone
	}
	stamp := candidates[0]
	for _, candidate := range candidates[1:] {
		if !candidate.Equal(stamp) {
			return time.Time{}, SourceTimeAmbiguous
		}
	}
	if stamp.IsZero() {
		return time.Time{}, SourceTimeUndated
	}
	return stamp, SourceTimeFound
}

// TextCandidates returns the starting timestamp for each matching sequence of source text.
func (index *SourceTimeIndex) TextCandidates(role string, text string) []time.Time {
	trimmed := strings.TrimSpace(text)
	if index == nil || trimmed == "" {
		return nil
	}
	candidates := make([]time.Time, 0)
	for _, run := range index.runs[role] {
		for start := range run {
			if runSpells(run[start:], trimmed) {
				candidates = append(candidates, run[start].timestamp)
			}
		}
	}
	return candidates
}

// runSpells reports whether consecutive entries match text with whitespace between them.
func runSpells(run []sourceText, text string) bool {
	remaining := text
	for position, entry := range run {
		if position > 0 {
			separated := strings.TrimLeftFunc(remaining, unicode.IsSpace)
			if len(separated) == len(remaining) {
				return false
			}
			remaining = separated
		}
		rest, cut := strings.CutPrefix(remaining, entry.text)
		if !cut {
			return false
		}
		if rest == "" {
			return true
		}
		remaining = rest
	}
	return false
}
