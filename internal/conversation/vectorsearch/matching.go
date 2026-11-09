package vectorsearch

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/transcript"
	"goodkind.io/lm-semantic-search/collection"
)

func newSourceTimeLookup(source TimestampSourceRepairSource) sourceTimeLookup {
	lookup := sourceTimeLookup{
		texts:                nil,
		thinking:             map[string][]int64{},
		fields:               map[string][]sourceField{},
		matchText:            source.MatchText,
		toolCalls:            source.ToolCalls,
		toolCallsComplete:    source.ToolCallsComplete,
		missingNativeRecords: source.MissingNativeRecords,
		toolBindings:         map[string]toolBinding{},
		groupedUsers:         source.GroupedUsers,
		actualUsers:          source.ActualUsers,
		transcript:           source.Transcript,
		emptySource: len(source.Documents) == 0 && len(source.CopyDocuments) == 0 &&
			len(source.ToolCalls) == 0,
		corrections:  source.Corrections,
		storedDates:  nil,
		boundedDates: make(map[int64]bool, len(source.ComposerBoundedDates)),
	}
	for _, stamp := range source.ComposerBoundedDates {
		lookup.boundedDates[stamp] = true
	}

	entries := lookup.indexDocuments(source.Documents)
	copies := lookup.indexDocuments(source.CopyDocuments)
	lookup.texts = transcript.NewSourceTimeIndex(entries, copies)

	return lookup
}

// indexDocuments indexes document text and thinking and returns text entries.
func (lookup sourceTimeLookup) indexDocuments(
	docs []semsearch.SemDoc,
) []transcript.SourceTimeEntry {
	entries := make([]transcript.SourceTimeEntry, 0, len(docs))
	for _, doc := range docs {
		stamp := time.Time{}
		if doc.TimestampUnix != missingTimestampUnix {
			stamp = time.Unix(doc.TimestampUnix, 0).UTC()
		}

		entries = append(
			entries,
			transcript.SourceTimeEntry{Role: doc.Role, Text: doc.Text, Timestamp: stamp},
		)
		if strings.TrimSpace(doc.Text) != "" {
			key := sourceRepairKey(doc.Role, conversationTextFamily)
			lookup.fields[key] = append(
				lookup.fields[key],
				sourceField{text: doc.Text, timestampUnix: doc.TimestampUnix},
			)
		}

		if strings.TrimSpace(doc.Thinking) != "" {
			key := sourceRepairKey(doc.Role, conversationThinkingFamily)
			lookup.fields[key] = append(
				lookup.fields[key],
				sourceField{text: doc.Thinking, timestampUnix: doc.TimestampUnix},
			)
		}

		if thinking := strings.TrimSpace(doc.Thinking); thinking != "" {
			key := sourceRepairKey(doc.Role, thinking)
			lookup.thinking[key] = append(lookup.thinking[key], doc.TimestampUnix)
		}
	}

	return entries
}

func sourceRepairKey(role string, content string) string {
	return role + sourceRepairKeySeparator + strings.TrimSpace(content)
}

// fragmentCandidates returns source timestamps matching a stored row fragment.
func (lookup sourceTimeLookup) fragmentCandidates(
	familyKind string,
	role string,
	row collection.StoredRow,
) []int64 {
	fields := lookup.fields[sourceRepairKey(role, familyKind)]
	candidates := make([]int64, 0)
	if row.SplitPartRecorded && row.SplitPart > 0 {
		offset := int(row.SplitPart) - 1

		for _, field := range fields {
			if offset+len(row.Content) <= len(field.text) &&
				field.text[offset:offset+len(row.Content)] == row.Content {
				candidates = append(candidates, field.timestampUnix)
			}
		}

		return candidates
	}

	fragment := row.Content
	if familyKind == conversationTextFamily && lookup.matchText != nil {
		fragment = lookup.matchText(role, fragment)
	}

	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return nil
	}

	for _, field := range fields {
		if strings.TrimSpace(field.text) == fragment {
			candidates = append(candidates, field.timestampUnix)
		}
	}

	return candidates
}

func (lookup sourceTimeLookup) fragmentContained(
	familyKind string,
	role string,
	row collection.StoredRow,
) bool {
	fragment := row.Content
	if familyKind == conversationTextFamily && lookup.matchText != nil {
		fragment = lookup.matchText(role, fragment)
	}

	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return false
	}

	for _, field := range lookup.fields[sourceRepairKey(role, familyKind)] {
		if strings.Contains(field.text, fragment) {
			return true
		}
	}

	return false
}

// rowRole returns the first nonempty role in the family.
func rowRole(members []collection.StoredRow) string {
	for _, row := range members {
		if cell, found := row.Scalars[roleColumn]; found &&
			cell.State == collection.ScalarCellValue &&
			cell.Value.String != "" {
			return cell.Value.String
		}
	}

	return ""
}

// familyText joins complete family paths and removes repeated tool names.
func familyText(key string, familyKind string, members []collection.StoredRow) (string, bool) {
	byPath := make(map[string][]collection.StoredRow)

	for _, row := range members {
		byPath[row.RelativePath] = append(byPath[row.RelativePath], row)
	}

	if rows, single := byPath[key]; single {
		if len(byPath) != 1 {
			return "", false
		}

		return pathText(rows)
	}

	parts := make([]string, 0, len(byPath))
	for index := range len(byPath) {
		rows, found := byPath[key+familyPartSeparator+strconv.Itoa(index)]
		if !found {
			return "", false
		}

		text, complete := pathText(rows)
		if !complete {
			return "", false
		}

		parts = append(parts, text)
	}

	if familyKind != conversationToolFamily {
		return strings.Join(parts, ""), true
	}

	name, _, _ := strings.Cut(parts[0], toolContinuationBreak)
	for index := 1; index < len(parts); index++ {
		rest, cut := strings.CutPrefix(parts[index], name+toolContinuationBreak)
		if !cut {
			return "", false
		}

		parts[index] = rest
	}

	return strings.Join(parts, ""), true
}

// pathText joins contiguous stored split parts in their recorded order.
func pathText(rows []collection.StoredRow) (string, bool) {
	if len(rows) == 1 && (!rows[0].SplitPartRecorded || rows[0].SplitPart == 0) {
		return rows[0].Content, true
	}

	ordered := slices.Clone(rows)
	slices.SortFunc(
		ordered,
		func(left collection.StoredRow, right collection.StoredRow) int {
			return int(left.SplitPart) - int(right.SplitPart)
		},
	)
	var builder strings.Builder
	expected := 1

	for _, row := range ordered {
		if !row.SplitPartRecorded || int(row.SplitPart) != expected || row.Content == "" {
			return "", false
		}

		builder.WriteString(row.Content)
		expected += len(row.Content)
	}

	return builder.String(), true
}
