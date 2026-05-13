package compact

import (
	"fmt"
	"strings"
)

// Rehydrate expands prior synthetic summaries into virtual chat entries
// so planning and synthesis can operate on ordinary PostBoundary rows.
func Rehydrate(slice *Slice, maxLayers int) *Slice {
	if slice == nil || maxLayers <= 0 {
		return slice
	}
	for pass := range maxLayers {
		entryIndex, summary, ok := nextSyntheticSummary(slice.PostBoundary)
		if !ok {
			return slice
		}
		virtualEntries := buildRehydratedEntries(entryIndex, slice.PostBoundary[entryIndex], summary)
		slice.PostBoundary = spliceEntries(slice.PostBoundary, entryIndex, virtualEntries)
		slice.PairIndex = buildPairIndex(slice.PostBoundary)
		compactLog.Logger().Debug("compact.rehydrate.expanded",
			"component", "compact",
			"subcomponent", "rehydrate",
			"entry_index", entryIndex,
			"virtual_count", len(virtualEntries),
			"layers_remaining", maxLayers-pass-1,
		)
	}
	return slice
}

func nextSyntheticSummary(entries []Entry) (int, *syntheticSummary, bool) {
	for entryIndex, entry := range entries {
		summary, ok := parseSyntheticSummary(entry)
		if ok {
			return entryIndex, summary, true
		}
	}
	return -1, nil, false
}

func buildRehydratedEntries(entryIndex int, source Entry, summary *syntheticSummary) []Entry {
	sourceCopy := source
	virtualEntries := make([]Entry, 0, len(summary.DropOrder()))
	for _, chunkKey := range summary.DropOrder() {
		text, ok := rehydratedTextForChunk(summary, chunkKey)
		if !ok {
			continue
		}
		virtualEntry := Entry{
			LineIndex:          0,
			Raw:                nil,
			UUID:               "",
			ParentUUID:         "",
			Type:               "user",
			Subtype:            "",
			Timestamp:          source.Timestamp,
			IsMeta:             false,
			IsSummary:          false,
			Role:               "user",
			Content:            nil,
			TextOnly:           text,
			RehydratedFrom:     entryIndex,
			RehydratedChunkKey: chunkKey,
			RehydratedSource:   nil,
		}
		if len(virtualEntries) == 0 {
			virtualEntry.RehydratedSource = &sourceCopy
		}
		virtualEntries = append(virtualEntries, virtualEntry)
	}
	return virtualEntries
}

func rehydratedTextForChunk(summary *syntheticSummary, chunkKey string) (string, bool) {
	if chunkKey == summaryChunkContinue {
		if !summary.HasContinue {
			return "", false
		}
		return "## Continue from here.\n", true
	}
	if chunkKey == summaryChunkSummary {
		return summary.DroppedSummary, summary.DroppedSummary != ""
	}
	if chunkKey == summaryChunkWhat {
		return summary.WhatDropped, summary.WhatDropped != ""
	}
	if chunkKey == summaryChunkContinuity {
		return summary.Continuity, true
	}
	if strings.HasPrefix(chunkKey, "tool_item:") {
		var toolIndex int
		if _, err := fmt.Sscanf(chunkKey, "tool_item:%d", &toolIndex); err != nil {
			return "", false
		}
		if toolIndex < 0 || toolIndex >= len(summary.ToolItems) {
			return "", false
		}
		return summary.ToolItems[toolIndex], true
	}
	if strings.HasPrefix(chunkKey, "surviving_turn:") {
		return rehydratedTextForTurnChunk(summary, chunkKey)
	}
	return "", false
}

func rehydratedTextForTurnChunk(summary *syntheticSummary, chunkKey string) (string, bool) {
	var turnIndex int
	var partIndex int
	partFormat := "surviving_turn:%d" + syntheticTurnPartPrefix + "%d"
	if strings.Contains(chunkKey, syntheticTurnPartPrefix) {
		if _, err := fmt.Sscanf(chunkKey, partFormat, &turnIndex, &partIndex); err != nil {
			return "", false
		}
		if turnIndex < 0 || turnIndex >= len(summary.TranscriptTurns) {
			return "", false
		}
		prefix, parts := splitSyntheticTranscriptTurn(summary.TranscriptTurns[turnIndex])
		if partIndex < 0 || partIndex >= len(parts) {
			return "", false
		}
		return prefix + parts[partIndex], true
	}
	if _, err := fmt.Sscanf(chunkKey, "surviving_turn:%d", &turnIndex); err != nil {
		return "", false
	}
	if turnIndex < 0 || turnIndex >= len(summary.TranscriptTurns) {
		return "", false
	}
	return summary.TranscriptTurns[turnIndex], true
}

func spliceEntries(entries []Entry, entryIndex int, replacement []Entry) []Entry {
	out := make([]Entry, 0, len(entries)-1+len(replacement))
	out = append(out, entries[:entryIndex]...)
	out = append(out, replacement...)
	out = append(out, entries[entryIndex+1:]...)
	return out
}
