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
	// SourceTimeCombined indicates that matches join multiple source entries.
	SourceTimeCombined
)

// SourceTimeCandidate contains a matching source timestamp and its combination status.
type SourceTimeCandidate struct {
	Timestamp time.Time
	// Combined reports whether the match joins multiple source entries.
	Combined bool
}

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
	// copies indexes individual stored copies by role and text.
	copies map[string][]time.Time
}

// NewSourceTimeIndex groups nonempty source text into consecutive role runs.
func NewSourceTimeIndex(entries []SourceTimeEntry, copies []SourceTimeEntry) *SourceTimeIndex {
	index := &SourceTimeIndex{runs: map[string][][]sourceText{}, copies: map[string][]time.Time{}}
	for _, entry := range copies {
		trimmed := strings.TrimSpace(entry.Text)
		if trimmed == "" {
			continue
		}
		key := copyKey(entry.Role, trimmed)
		index.copies[key] = append(index.copies[key], entry.Timestamp)
	}
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
	candidates := make([]time.Time, 0)
	combined := false
	for _, candidate := range index.TextCandidates(role, text) {
		if candidate.Combined {
			combined = true
			continue
		}
		candidates = append(candidates, candidate.Timestamp)
	}
	if len(candidates) == 0 {
		if combined {
			return time.Time{}, SourceTimeCombined
		}
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
func (index *SourceTimeIndex) TextCandidates(role string, text string) []SourceTimeCandidate {
	trimmed := strings.TrimSpace(text)
	if index == nil || trimmed == "" {
		return nil
	}
	candidates := make([]SourceTimeCandidate, 0)
	for _, run := range index.runs[role] {
		for start := range run {
			if used := runSpells(run[start:], trimmed); used > 0 {
				candidates = append(candidates, SourceTimeCandidate{Timestamp: run[start].timestamp, Combined: used > 1})
			}
		}
	}
	for _, stamp := range index.copies[copyKey(role, trimmed)] {
		candidates = append(candidates, SourceTimeCandidate{Timestamp: stamp, Combined: false})
	}
	return candidates
}

func copyKey(role string, text string) string {
	return role + "\x00" + text
}

// runSpells returns the number of consecutive entries that spell text.
func runSpells(run []sourceText, text string) int {
	remaining := text
	for position, entry := range run {
		if position > 0 {
			separated := strings.TrimLeftFunc(remaining, unicode.IsSpace)
			if len(separated) == len(remaining) {
				return 0
			}
			remaining = separated
		}
		rest, cut := strings.CutPrefix(remaining, entry.text)
		if !cut {
			return 0
		}
		if rest == "" {
			return position + 1
		}
		remaining = rest
	}
	return 0
}
