package compact

// Dehydrate is a placeholder pass-through that returns the input slice
// unchanged. It exists so callers can be wired up in CLYDE-414 ahead of
// the real recomposition logic that lands in CLYDE-416. The opts
// argument carries the drop bookkeeping the future implementation will
// consult; today its sizes are only recorded in a debug slog event so
// the wiring is observable.
func Dehydrate(slice *Slice, opts SynthOptions) *Slice {
	compactLog.Logger().Debug("compact.dehydrate.no_op",
		"component", "compact",
		"subcomponent", "dehydrate",
		"dropped_chat_entries", len(opts.DroppedChatEntries),
		"dropped_summary_chunks", len(opts.DroppedSummaryChunks),
	)
	return slice
}
