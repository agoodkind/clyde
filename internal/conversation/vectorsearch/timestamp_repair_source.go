package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/transcript"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	sourceRepairKeySeparator = "\x00"
	familyPartSeparator      = "/"
	toolContinuationBreak    = "\n"
)

// TimestampSourceRepairSource supplies documents and text matching for one conversation.
type TimestampSourceRepairSource struct {
	ConversationID string
	Documents      []semsearch.SemDoc
	// MatchText normalizes stored text before matching it against source text.
	MatchText func(role string, text string) string
}

// TimestampSourceRepairReport records source repair counts and matching evidence.
type TimestampSourceRepairReport struct {
	TimestampRepairReport
	// RowsNotZeroTime counts rows that do not have the zero timestamp.
	RowsNotZeroTime int `json:"rows_not_zero_time"`
	// RowsEmptyContent counts zero timestamp rows with empty family content.
	RowsEmptyContent int `json:"rows_empty_content"`
	// RowsIncomplete counts zero timestamp rows whose family content cannot be joined.
	RowsIncomplete int `json:"rows_incomplete"`
	// Evidence records the matching outcome for each family with zero timestamp rows.
	Evidence []SourceRepairEvidence `json:"evidence,omitempty"`
}

// Add combines another report's counts with this report.
func (report *TimestampSourceRepairReport) Add(other TimestampSourceRepairReport) {
	report.TimestampRepairReport.Add(other.TimestampRepairReport)
	report.RowsNotZeroTime += other.RowsNotZeroTime
	report.RowsEmptyContent += other.RowsEmptyContent
	report.RowsIncomplete += other.RowsIncomplete
}

// RepairTimestampsFromSource matches zero timestamp rows against source documents.
// It writes matching timestamps unless dryRun is true.
func (c *Client) RepairTimestampsFromSource(
	ctx context.Context,
	collectionID string,
	sources []TimestampSourceRepairSource,
	dryRun bool,
) (TimestampSourceRepairReport, error) {
	report := TimestampSourceRepairReport{
		TimestampRepairReport: TimestampRepairReport{
			Conversations: 0, RowsRead: 0, RowsCurrent: 0, RowsChanged: 0, RowsWritten: 0,
			RowsUnmatched: 0, RowsMissingTimestamp: 0, RowsAmbiguous: 0, RowsInvalidMetadata: 0,
		},
		RowsNotZeroTime: 0, RowsEmptyContent: 0, RowsIncomplete: 0, Evidence: nil,
	}
	if c == nil {
		return report, errors.New("conversation search client is nil")
	}
	milvus, isMilvus := c.store.(*milvusBackend)
	if !isMilvus {
		return report, ErrTimestampRepairUnsupported
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return report, errors.New("collection ID is empty")
	}
	collectionName := CollectionName(trimmedCollectionID)
	exists, err := c.loadCollectionIfPresent(ctx, collectionName)
	if err != nil {
		return report, err
	}
	if !exists {
		slog.WarnContext(ctx, "conversation.vectorsearch.source_timestamp_repair_collection_absent",
			"concern", "conversation.semantic", "component", "conversation", "collection", collectionName)
		return report, fmt.Errorf("collection does not exist %s: %w", collectionName, ErrCollectionAbsent)
	}
	matched := make(map[string]matchedRow)
	for _, source := range sources {
		conversationID := strings.TrimSpace(source.ConversationID)
		if conversationID == "" {
			return report, errors.New("source conversation ID is empty")
		}
		report.Conversations++
		rows, queryErr := c.store.QueryRows(ctx, collection.RowsRequest{
			Collection:  collectionName,
			Declaration: sourceRepairRowsDeclaration(),
			ItemIDs:     []string{conversationID},
			PathPrefixes: []string{
				conversationRelativePathPrefix(conversationID),
				conversationToolRelativePathPrefix(conversationID),
				conversationThinkingRelativePathPrefix(conversationID),
			},
			IncludeVector: false,
		})
		if queryErr != nil {
			return report, failRead("read conversation rows from collection "+collectionName, queryErr)
		}
		lookup := newSourceTimeLookup(source)
		matchSourceFamilies(conversationID, rows, lookup, matched, &report)
	}
	updates, err := plannedTimestampUpdates(ctx, milvus, collectionName, matched, &report.TimestampRepairReport)
	if err != nil {
		return report, err
	}
	if !dryRun {
		if err := writeSourceTimestamps(ctx, c, milvus, collectionName, updates, &report.TimestampRepairReport); err != nil {
			return report, err
		}
	}
	slog.InfoContext(ctx, "conversation.vectorsearch.source_timestamp_repair_completed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"collection", collectionName,
		"shared_layout", milvus.shared != nil,
		"conversations", report.Conversations,
		"rows_read", report.RowsRead,
		"rows_not_zero_time", report.RowsNotZeroTime,
		"rows_empty_content", report.RowsEmptyContent,
		"rows_incomplete", report.RowsIncomplete,
		"rows_current", report.RowsCurrent,
		"rows_changed", report.RowsChanged,
		"rows_written", report.RowsWritten,
		"rows_unmatched", report.RowsUnmatched,
		"rows_missing_timestamp", report.RowsMissingTimestamp,
		"rows_ambiguous", report.RowsAmbiguous,
		"rows_invalid_metadata", report.RowsInvalidMetadata,
		"dry_run", dryRun,
	)
	return report, nil
}

// writeSourceTimestamps writes timestamp updates in batches and counts written rows.
func writeSourceTimestamps(ctx context.Context, c *Client, milvus *milvusBackend, collectionName string, updates []timestampUpdate, report *TimestampRepairReport) error {
	columns := declaredColumns(c.declaration, timestampUnixColumn)
	for start := 0; start < len(updates); start += timestampRepairWriteBatchRows {
		batch := updates[start:min(start+timestampRepairWriteBatchRows, len(updates))]
		if milvus.shared == nil {
			if err := milvus.writeRowTimestamps(ctx, collectionName, columns, batch); err != nil {
				return err
			}
			report.RowsWritten += len(batch)
			continue
		}
		rows := make([]collection.Row, 0, len(batch))
		for _, update := range batch {
			rows = append(rows, collection.Row{
				ID:                update.id,
				Content:           "",
				RelativePath:      update.relativePath,
				StartLine:         0,
				EndLine:           0,
				FileExtension:     "",
				Metadata:          update.metadata,
				SplitPart:         0,
				SplitPartRecorded: false,
				Vector:            nil,
				Scalars:           map[string]collection.ScalarValue{timestampUnixColumn: collection.Int64Scalar(update.timestampUnix)},
			})
		}
		written, err := milvus.shared.UpdateRowScalars(ctx, collectionName, columns, rows)
		report.RowsWritten += written
		if err != nil {
			return failRead("update shared row timestamps in "+collectionName, err)
		}
	}
	return nil
}

func sourceRepairRowsDeclaration() collection.Declaration {
	return collection.Declaration{
		ItemIDColumn: conversationIDColumn,
		Scalars: []collection.ScalarColumn{
			nullableStringColumn(conversationIDColumn, conversationIDMaxLength),
			nullableStringColumn(roleColumn, roleMaxLength),
			{Name: timestampUnixColumn, Type: collection.ScalarTypeInt64, Nullable: true, MaxLength: 0},
		},
	}
}

// sourceTimeLookup indexes source text, tool calls, and thinking by role and content.
type sourceTimeLookup struct {
	texts    *transcript.SourceTimeIndex
	tools    map[string][]int64
	thinking map[string][]int64
	// @@FIELDS_DOC@@
	fields    map[string][]sourceField
	matchText func(role string, text string) string
}

// @@SOURCE_FIELD_DOC@@
type sourceField struct {
	text          string
	timestampUnix int64
}

func newSourceTimeLookup(source TimestampSourceRepairSource) sourceTimeLookup {
	entries := make([]transcript.SourceTimeEntry, 0, len(source.Documents))
	lookup := sourceTimeLookup{texts: nil, tools: map[string][]int64{}, thinking: map[string][]int64{}, fields: map[string][]sourceField{}, matchText: source.MatchText}
	for _, doc := range source.Documents {
		stamp := time.Time{}
		if doc.TimestampUnix != missingTimestampUnix {
			stamp = time.Unix(doc.TimestampUnix, 0).UTC()
		}
		entries = append(entries, transcript.SourceTimeEntry{Role: doc.Role, Text: doc.Text, Timestamp: stamp})
		if strings.TrimSpace(doc.Text) != "" {
			key := sourceRepairKey(doc.Role, conversationTextFamily)
			lookup.fields[key] = append(lookup.fields[key], sourceField{text: doc.Text, timestampUnix: doc.TimestampUnix})
		}
		if strings.TrimSpace(doc.Thinking) != "" {
			key := sourceRepairKey(doc.Role, conversationThinkingFamily)
			lookup.fields[key] = append(lookup.fields[key], sourceField{text: doc.Thinking, timestampUnix: doc.TimestampUnix})
		}
		for _, tool := range doc.Tools {
			cleaned := semsearch.SemToolCall{
				Name:     strings.ToValidUTF8(tool.Name, ""),
				Display:  strings.ToValidUTF8(tool.Display, ""),
				LangHint: strings.ToValidUTF8(tool.LangHint, ""),
				Output:   "",
				IsError:  tool.IsError,
			}
			if content := toolContent(cleaned); content != "" {
				key := sourceRepairKey(doc.Role, content)
				lookup.tools[key] = append(lookup.tools[key], doc.TimestampUnix)
			}
		}
		if thinking := strings.TrimSpace(doc.Thinking); thinking != "" {
			key := sourceRepairKey(doc.Role, thinking)
			lookup.thinking[key] = append(lookup.thinking[key], doc.TimestampUnix)
		}
	}
	lookup.texts = transcript.NewSourceTimeIndex(entries)
	return lookup
}

func sourceRepairKey(role string, content string) string {
	return role + sourceRepairKeySeparator + strings.TrimSpace(content)
}

// familyCandidates returns source timestamps matching the family kind, role, and text.
func (lookup sourceTimeLookup) familyCandidates(familyKind string, role string, text string) []int64 {
	switch familyKind {
	case conversationToolFamily:
		return lookup.tools[sourceRepairKey(role, text)]
	case conversationThinkingFamily:
		return lookup.thinking[sourceRepairKey(role, text)]
	}
	matchText := text
	if lookup.matchText != nil {
		matchText = lookup.matchText(role, text)
	}
	stamps := lookup.texts.TextCandidates(role, matchText)
	candidates := make([]int64, 0, len(stamps))
	for _, stamp := range stamps {
		if stamp.IsZero() {
			candidates = append(candidates, missingTimestampUnix)
			continue
		}
		candidates = append(candidates, stamp.Unix())
	}
	return candidates
}

// @@FRAGMENT_CANDIDATES_DOC@@
func (lookup sourceTimeLookup) fragmentCandidates(familyKind string, role string, row collection.StoredRow) []int64 {
	fields := lookup.fields[sourceRepairKey(role, familyKind)]
	candidates := make([]int64, 0)
	if row.SplitPartRecorded && row.SplitPart > 0 {
		offset := int(row.SplitPart) - 1
		for _, field := range fields {
			if offset+len(row.Content) <= len(field.text) && field.text[offset:offset+len(row.Content)] == row.Content {
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
		if strings.Contains(field.text, fragment) {
			candidates = append(candidates, field.timestampUnix)
		}
	}
	return candidates
}

// distinctCandidates sorts and deduplicates timestamps and identifies undated matches.
func distinctCandidates(candidates []int64) ([]int64, bool) {
	distinct := slices.Clone(candidates)
	slices.Sort(distinct)
	distinct = slices.Compact(distinct)
	if len(distinct) == 1 && distinct[0] == missingTimestampUnix {
		return nil, true
	}
	return distinct, false
}

const (
	conversationTextFamily     = "conv"
	conversationToolFamily     = "convtool"
	conversationThinkingFamily = "convthink"
)

// Evidence outcomes for one stored row family.
const (
	SourceOutcomeMatched    = "matched"
	SourceOutcomeAmbiguous  = "ambiguous"
	SourceOutcomeUnmatched  = "unmatched"
	SourceOutcomeUndated    = "undated"
	SourceOutcomeEmpty      = "empty_content"
	SourceOutcomeIncomplete = "incomplete"
)

// SourceRepairEvidence records the outcome and candidates for one stored row family.
type SourceRepairEvidence struct {
	ConversationID string   `json:"conversation_id"`
	Family         string   `json:"family"`
	Rows           []string `json:"rows"`
	Outcome        string   `json:"outcome"`
	Candidates     []int64  `json:"candidates,omitempty"`
	Chosen         int64    `json:"chosen,omitempty"`
	// @@MATCH_KIND_DOC@@
	Match string `json:"match,omitempty"`
}

type pendingFamily struct {
	order    []int
	rows     []collection.StoredRow
	evidence *SourceRepairEvidence
}

// matchSourceFamilies groups rows into families, classifies them, and records outcomes.
func matchSourceFamilies(conversationID string, rows []collection.StoredRow, lookup sourceTimeLookup, matched map[string]matchedRow, report *TimestampSourceRepairReport) {
	families := make(map[string][]collection.StoredRow)
	for _, row := range rows {
		report.RowsRead++
		key := chunkFamilyKey(conversationID, row.RelativePath)
		families[key] = append(families[key], row)
	}
	ordered := make([]pendingFamily, 0, len(families))
	for key, members := range families {
		groups := [][]collection.StoredRow{members}
		if wholeVersions(key, members) {
			groups = make([][]collection.StoredRow, 0, len(members))
			for _, row := range members {
				groups = append(groups, []collection.StoredRow{row})
			}
		}
		for _, group := range groups {
			family, dated := classifyFamily(conversationID, key, group, lookup)
			report.RowsNotZeroTime += dated
			if family == nil {
				continue
			}
			ordered = append(ordered, fragmentFallback(conversationID, key, *family, lookup)...)
		}
	}
	slices.SortFunc(ordered, func(left pendingFamily, right pendingFamily) int {
		return slices.Compare(left.order, right.order)
	})
	for _, family := range ordered {
		tallyFamily(family, matched, report)
	}
}

// classifyFamily matches a family's zero timestamp rows and counts its dated rows.
func classifyFamily(conversationID string, key string, members []collection.StoredRow, lookup sourceTimeLookup) (*pendingFamily, int) {
	zeroTime := make([]collection.StoredRow, 0, len(members))
	ids := make([]string, 0, len(members))
	for _, row := range members {
		if rowHasZeroTime(row) {
			zeroTime = append(zeroTime, row)
			ids = append(ids, row.ID)
		}
	}
	dated := len(members) - len(zeroTime)
	if len(zeroTime) == 0 {
		return nil, dated
	}
	slices.Sort(ids)
	evidence := &SourceRepairEvidence{ConversationID: conversationID, Family: key, Rows: ids, Outcome: "", Candidates: nil, Chosen: 0, Match: ""}
	family := &pendingFamily{order: familyOrder(key), rows: zeroTime, evidence: evidence}
	familyKind, _, _ := strings.Cut(key, familyPartSeparator)
	text, complete := familyText(key, familyKind, members)
	if !complete {
		evidence.Outcome = SourceOutcomeIncomplete
		return family, dated
	}
	if strings.TrimSpace(text) == "" {
		evidence.Outcome = SourceOutcomeEmpty
		return family, dated
	}
	candidates, undated := distinctCandidates(lookup.familyCandidates(familyKind, rowRole(members), text))
	evidence.Candidates = candidates
	switch {
	case undated:
		evidence.Outcome = SourceOutcomeUndated
	case len(candidates) == 1:
		evidence.Outcome = SourceOutcomeMatched
		evidence.Chosen = candidates[0]
	case len(candidates) > 1:
		evidence.Outcome = SourceOutcomeAmbiguous
	default:
		evidence.Outcome = SourceOutcomeUnmatched
	}
	return family, dated
}

// @@FRAGMENT_FALLBACK_DOC@@
func fragmentFallback(conversationID string, key string, family pendingFamily, lookup sourceTimeLookup) []pendingFamily {
	familyKind, _, _ := strings.Cut(key, familyPartSeparator)
	outcome := family.evidence.Outcome
	if familyKind == conversationToolFamily || (outcome != SourceOutcomeUnmatched && outcome != SourceOutcomeIncomplete) {
		return []pendingFamily{family}
	}
	role := rowRole(family.rows)
	split := make([]pendingFamily, 0, len(family.rows))
	for _, row := range family.rows {
		evidence := &SourceRepairEvidence{ConversationID: conversationID, Family: row.RelativePath, Rows: []string{row.ID}, Outcome: "", Candidates: nil, Chosen: 0, Match: sourceMatchFragment}
		if strings.TrimSpace(row.Content) == "" {
			evidence.Outcome = SourceOutcomeEmpty
			split = append(split, pendingFamily{order: family.order, rows: []collection.StoredRow{row}, evidence: evidence})
			continue
		}
		candidates, undated := distinctCandidates(lookup.fragmentCandidates(familyKind, role, row))
		evidence.Candidates = candidates
		switch {
		case undated:
			evidence.Outcome = SourceOutcomeUndated
		case len(candidates) == 1:
			evidence.Outcome = SourceOutcomeMatched
			evidence.Chosen = candidates[0]
		case len(candidates) > 1:
			evidence.Outcome = SourceOutcomeAmbiguous
		default:
			evidence.Outcome = SourceOutcomeUnmatched
		}
		split = append(split, pendingFamily{order: family.order, rows: []collection.StoredRow{row}, evidence: evidence})
	}
	return split
}

// @@SOURCE_MATCH_FRAGMENT_DOC@@
const sourceMatchFragment = "fragment"

// tallyFamily records evidence and counts or selects rows according to the outcome.
func tallyFamily(family pendingFamily, matched map[string]matchedRow, report *TimestampSourceRepairReport) {
	report.Evidence = append(report.Evidence, *family.evidence)
	count := len(family.rows)
	switch family.evidence.Outcome {
	case SourceOutcomeMatched:
		for _, row := range family.rows {
			matched[row.ID] = matchedRow{relativePath: row.RelativePath, timestampUnix: family.evidence.Chosen}
		}
	case SourceOutcomeAmbiguous:
		report.RowsAmbiguous += count
	case SourceOutcomeUndated:
		report.RowsMissingTimestamp += count
	case SourceOutcomeEmpty:
		report.RowsEmptyContent += count
	case SourceOutcomeIncomplete:
		report.RowsIncomplete += count
	default:
		report.RowsUnmatched += count
	}
}

// familyOrder returns the numeric ordering key for a family path.
func familyOrder(key string) []int {
	parts := strings.Split(key, familyPartSeparator)
	order := make([]int, 0, 3)
	for _, part := range parts[2:] {
		value, err := strconv.Atoi(part)
		if err != nil {
			value = -1
		}
		order = append(order, value)
	}
	kindOrder := map[string]int{conversationTextFamily: 0, conversationThinkingFamily: 1, conversationToolFamily: 2}
	if len(order) == 1 {
		order = append(order, kindOrder[parts[0]], 0)
	} else if len(order) > 1 {
		order = []int{order[0], kindOrder[parts[0]], order[1]}
	}
	return order
}

// wholeVersions reports whether a stored row family contains at least two rows
// and every row uses the family path itself and has a split part of zero or
// does not have a recorded split part. Each row stores the complete message
// text from an earlier or later write. The caller matches each row against its
// own text instead of joining the rows as parts of one message.
func wholeVersions(key string, members []collection.StoredRow) bool {
	if len(members) < 2 {
		return false
	}
	for _, row := range members {
		if row.RelativePath != key || (row.SplitPartRecorded && row.SplitPart != 0) {
			return false
		}
	}
	return true
}

func rowHasZeroTime(row collection.StoredRow) bool {
	cell, found := row.Scalars[timestampUnixColumn]
	return found && cell.State == collection.ScalarCellValue && cell.Value.Int64 == missingTimestampUnix
}

// rowRole returns the first nonempty role in the family.
func rowRole(members []collection.StoredRow) string {
	for _, row := range members {
		if cell, found := row.Scalars[roleColumn]; found && cell.State == collection.ScalarCellValue && cell.Value.String != "" {
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
	slices.SortFunc(ordered, func(left collection.StoredRow, right collection.StoredRow) int {
		return int(left.SplitPart) - int(right.SplitPart)
	})
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
