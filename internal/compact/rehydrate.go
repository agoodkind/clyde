package compact

// Rehydrate is a placeholder pass-through that returns the input slice
// unchanged. It exists so callers can be wired up in CLYDE-413 ahead of
// the real decomposition logic that lands in CLYDE-415. The maxLayers
// parameter is the upper bound on how many decomposition layers the
// future implementation will be allowed to walk; today it is only
// recorded in a debug slog event so the wiring is observable.
func Rehydrate(slice *Slice, maxLayers int) *Slice {
	compactLog.Logger().Debug("compact.rehydrate.no_op",
		"component", "compact",
		"subcomponent", "rehydrate",
		"max_layers", maxLayers,
	)
	return slice
}
