package compact

import "time"

// Dehydrate maps virtual rehydrated drops back onto the original
// synthetic entry and collapses virtual runs back to their source entry.
func Dehydrate(slice *Slice, opts SynthOptions) *Slice {
	if slice == nil || !hasRehydratedEntries(slice.PostBoundary) {
		return slice
	}
	virtualEntriesCollapsed := countRehydratedEntries(slice.PostBoundary)
	chunkKeysMapped := mapRehydratedDrops(slice, opts)
	slice.PostBoundary = collapseRehydratedEntries(slice.PostBoundary)
	slice.PairIndex = buildPairIndex(slice.PostBoundary)
	compactLog.Logger().Debug("compact.dehydrate.collapsed",
		"component", "compact",
		"subcomponent", "dehydrate",
		"virtual_entries_collapsed", virtualEntriesCollapsed,
		"chunk_keys_mapped", chunkKeysMapped,
	)
	return slice
}

func hasRehydratedEntries(entries []Entry) bool {
	for _, entry := range entries {
		if entry.RehydratedFrom >= 0 {
			return true
		}
	}
	return false
}

func countRehydratedEntries(entries []Entry) int {
	count := 0
	for _, entry := range entries {
		if entry.RehydratedFrom >= 0 {
			count++
		}
	}
	return count
}

func mapRehydratedDrops(slice *Slice, opts SynthOptions) int {
	if opts.DroppedChatEntries == nil {
		return 0
	}
	if opts.DroppedSummaryChunks == nil {
		opts.DroppedSummaryChunks = map[int]map[string]bool{}
	}
	chunkKeysMapped := 0
	for entryIndex := range opts.DroppedChatEntries {
		if entryIndex < 0 || entryIndex >= len(slice.PostBoundary) {
			continue
		}
		entry := slice.PostBoundary[entryIndex]
		if entry.RehydratedFrom < 0 {
			continue
		}
		if opts.DroppedSummaryChunks[entry.RehydratedFrom] == nil {
			opts.DroppedSummaryChunks[entry.RehydratedFrom] = map[string]bool{}
		}
		opts.DroppedSummaryChunks[entry.RehydratedFrom][entry.RehydratedChunkKey] = true
		delete(opts.DroppedChatEntries, entryIndex)
		chunkKeysMapped++
	}
	return chunkKeysMapped
}

func collapseRehydratedEntries(entries []Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	for entryIndex := 0; entryIndex < len(entries); {
		entry := entries[entryIndex]
		if entry.RehydratedFrom < 0 {
			out = append(out, entry)
			entryIndex++
			continue
		}
		source := rehydratedSourceForRun(entries, entryIndex)
		out = append(out, source)
		sourceIndex := entry.RehydratedFrom
		for entryIndex < len(entries) && entries[entryIndex].RehydratedFrom == sourceIndex {
			entryIndex++
		}
	}
	return out
}

func rehydratedSourceForRun(entries []Entry, startIndex int) Entry {
	sourceIndex := entries[startIndex].RehydratedFrom
	for entryIndex := startIndex; entryIndex < len(entries); entryIndex++ {
		entry := entries[entryIndex]
		if entry.RehydratedFrom != sourceIndex {
			break
		}
		if entry.RehydratedSource != nil {
			return *entry.RehydratedSource
		}
	}
	return Entry{
		LineIndex:          0,
		Raw:                nil,
		UUID:               "",
		ParentUUID:         "",
		Type:               "",
		Subtype:            "",
		Timestamp:          time.Time{},
		IsMeta:             false,
		IsSummary:          false,
		Role:               "",
		Content:            nil,
		TextOnly:           "",
		RehydratedFrom:     -1,
		RehydratedChunkKey: "",
		RehydratedSource:   nil,
	}
}
