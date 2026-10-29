package conversation

import (
	"context"
	"strings"
)

// visibleRecords returns sorted scanned records and retained records of conversations with deleted transcripts for listing, paging, lookup, and search scoping. It omits retained records with a matching scanned ID and applies the subagent visibility setting. Callers must lock idx.mu.
func (idx *Index) visibleRecords() []Record {
	if len(idx.retained) == 0 {
		return idx.visibleFrom(idx.records)
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
	return idx.visibleFrom(served)
}

// visibleScannedRecords returns scanned records under the subagent setting. The semantic sync feeder reads these records.
func (idx *Index) visibleScannedRecords() []Record {
	return idx.visibleFrom(idx.records)
}

// visibleFrom applies the subagent setting to records.
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
	idx.mu.Lock()
	records := idx.visibleRecords()
	idx.mu.Unlock()
	return records
}
