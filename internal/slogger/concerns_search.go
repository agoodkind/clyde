package slogger

// Search subsystem concern constants. The conversation semantic
// search pipeline (embedding pre-filter, sweep, rerank, large-model
// verification) emits both dotted-prefix events (search.embed.*,
// search.sweep_layer.*) and historical undotted messages
// ("llm request", "swapping model", "sweep complete", etc.). The
// router prefix-matches both shapes into this concern.
const (
	ConcernSearch = "search"
)
