package conversation

import (
	"context"
	"sort"
	"strings"

	"goodkind.io/clyde/internal/providerid"
)

// visibleRecords returns sorted scanned records and retained records for deleted transcripts, omitting retained records with matching scanned IDs and applying the subagent visibility setting. Callers must lock idx.mu.
func (idx *Index) visibleRecords() []Record {
	return idx.visibleFrom(idx.allRecords())
}

// allRecords merges scanned and retained records. Callers must lock idx.mu.
func (idx *Index) allRecords() []Record {
	if len(idx.retained) == 0 {
		return cloneRecords(idx.records)
	}

	served := make([]Record, 0, len(idx.records)+len(idx.retained))
	served = append(served, idx.records...)

	scanned := make(map[string]bool, len(idx.records))
	for _, record := range idx.records {
		scanned[record.ID] = true
	}

	for _, record := range idx.retained {
		if !scanned[record.ID] {
			served = append(served, record)
		}
	}

	sortRecords(served)

	return served
}

// visibleScannedRecords returns scanned records under the subagent setting. The semantic sync feeder reads these records.
func (idx *Index) visibleScannedRecords() []Record {
	return idx.visibleFrom(idx.records)
}

func (idx *Index) visibleFrom(records []Record) []Record {
	if idx.includeSubagents {
		return cloneRecords(records)
	}

	out := make([]Record, 0, len(records))
	for _, record := range records {
		if record.IsSubagent() {
			continue
		}

		out = append(out, record)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// ListWithStamps returns cached records and artifact stamps without a refresh.
func (idx *Index) ListWithStamps(ctx context.Context) ([]StampedRecord, error) {
	if err := idx.loadOnce(); err != nil {
		return nil, err
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	return cloneStampedRecords(idx.visibleScannedRecords(), idx.prevStamps), nil
}

// RecordByID returns the cached record with the exact id from the in-memory
// snapshot, taking no refresh. The second result is false when no record
// matches.
func (idx *Index) RecordByID(id string) (Record, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return emptyRecord(), false
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()
	// A hidden record is skipped rather than returned as a miss, so a subagent
	// conversation that somehow shares an id with a real one can never mask it.
	for _, records := range [][]Record{idx.records, idx.retained} {
		for _, record := range records {
			if record.ID != id {
				continue
			}

			if record.IsSubagent() && !idx.includeSubagents {
				continue
			}

			return record, true
		}
	}

	return emptyRecord(), false
}

func (idx *Index) recordsSnapshot() []Record {
	return idx.resolutionRecords(resolutionVisible)
}

func (idx *Index) resolutionRecords(scope resolutionScope) []Record {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if scope == resolutionExport {
		return idx.allRecords()
	}

	return idx.visibleRecords()
}

// recordByLatestRequestID finds the conversation with the given latest request
// id and reports how many conversations have that id. The comparison uses the
// whole string. A record with the id in another field does not match.
//
// Duplicated conversations can share the latest request id. In one Cursor
// store, 43 of 1,836 conversations with a latest request id shared it with
// another conversation. The caller needs the count to detect ambiguity.
func recordByLatestRequestID(records []Record, requestID string) (Record, int) {
	if requestID == "" {
		return emptyRecord(), 0
	}

	first := emptyRecord()
	carriers := 0

	for _, record := range records {
		if record.LatestRequestID != requestID {
			continue
		}

		if carriers == 0 {
			first = record
		}

		carriers++
	}

	return first, carriers
}

// recordByNativeID finds a record by provider and native conversation id.
//
// Provider resolvers return native ids. [resolveRecordExact] also compares
// titles and artifact paths across providers. A Cursor composer id pasted into
// a Claude chat can become that chat's title. Comparing both the provider and
// native id rejects that false match.
func recordByNativeID(
	records []Record,
	provider providerid.Provider,
	nativeID string,
) (Record, bool) {
	if nativeID == "" {
		return emptyRecord(), false
	}

	for _, record := range records {
		if record.Provider == provider && record.NativeID == nativeID {
			return record, true
		}
	}

	return emptyRecord(), false
}

func resolveRecord(records []Record, selector string) (Record, bool) {
	if record, ok := resolveRecordExact(records, selector); ok {
		return record, true
	}

	return resolveRecordFuzzyTitle(records, selector)
}

// resolveRecordExact compares a selector with record identifiers, artifact
// paths, and titles. Each comparison uses the whole string.
//
// [recordByLatestRequestID] counts request-id matches for [Index.Resolve].
// Matching request ids here would bypass that ambiguity check and disagree
// with resolve-request.
func resolveRecordExact(records []Record, selector string) (Record, bool) {
	if record, ok := resolveRecordIdentity(records, selector); ok {
		return record, true
	}

	for _, record := range records {
		if record.Title == selector {
			return record, true
		}
	}

	return emptyRecord(), false
}

// resolveRecordIdentity compares a selector with record identifiers and
// artifact paths, excluding titles.
//
// [Index.Resolve] uses this matcher for selectors shaped like request ids.
// A pasted request id can become a title without identifying the conversation
// that issued the request. Title matching would bypass request resolution.
func resolveRecordIdentity(records []Record, selector string) (Record, bool) {
	for _, record := range records {
		if record.ID == selector ||
			record.NativeID == selector ||
			record.ArtifactPath == selector {
			return record, true
		}
	}

	return emptyRecord(), false
}

// resolveRecordFuzzyTitle returns a record when exactly one title contains
// the selector as a substring, ignoring case.
//
// This matcher accepts partial titles. Selectors that identify one exact
// record must use an identity matcher.
func resolveRecordFuzzyTitle(records []Record, selector string) (Record, bool) {
	lower := strings.ToLower(selector)
	var matched Record
	matchCount := 0

	for _, record := range records {
		if strings.Contains(strings.ToLower(record.Title), lower) {
			matched = record
			matchCount++
		}
	}

	if matchCount != 1 {
		return emptyRecord(), false
	}

	return matched, true
}

func sortRecords(records []Record) {
	sort.SliceStable(
		records,
		func(i int, j int) bool {
			left := records[i]
			right := records[j]
			if !left.UpdatedAt.Equal(right.UpdatedAt) {
				return left.UpdatedAt.After(right.UpdatedAt)
			}

			return left.ID < right.ID
		},
	)
}

func cloneRecords(records []Record) []Record {
	if len(records) == 0 {
		return nil
	}

	out := make([]Record, len(records))
	copy(out, records)

	return out
}

func cloneStampedRecords(records []Record, stamps map[string]FileStamp) []StampedRecord {
	if len(records) == 0 {
		return nil
	}

	out := make([]StampedRecord, 0, len(records))
	for _, record := range records {
		out = append(
			out,
			StampedRecord{
				Record: record,
				Stamp:  stamps[recordKey(record.ArtifactPath, record.Selector)],
			},
		)
	}

	return out
}

// recordsByPath indexes records by artifact path and selector so the next
// incremental scan can reuse every conversation from an unchanged artifact.
func recordsByPath(records []Record) map[string]Record {
	byPath := make(map[string]Record, len(records))
	for _, record := range records {
		byPath[recordKey(record.ArtifactPath, record.Selector)] = record
	}

	return byPath
}

func foundRequestResolution(
	requestID string,
	origin RequestOrigin,
	record Record,
) RequestResolution {
	return RequestResolution{
		RequestID: requestID,
		Found:     true,
		Origin:    origin,
		Reason:    RequestNotFoundReasonUnspecified,
		Record:    record,
	}
}

func missingRequestResolution(requestID string, reason RequestNotFoundReason) RequestResolution {
	return RequestResolution{
		RequestID: requestID,
		Found:     false,
		Origin:    RequestOriginUnspecified,
		Reason:    reason,
		Record:    emptyRecord(),
	}
}
