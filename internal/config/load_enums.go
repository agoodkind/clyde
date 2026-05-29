package config

// codexReasoningSummary is the closed enum for
// `adapter.codex.reasoning_summary`. Empty string normalizes to auto.
type codexReasoningSummary string

const (
	codexReasoningSummaryAuto     codexReasoningSummary = "auto"
	codexReasoningSummaryConcise  codexReasoningSummary = "concise"
	codexReasoningSummaryDetailed codexReasoningSummary = "detailed"
	codexReasoningSummaryNone     codexReasoningSummary = "none"
)

// loggingIncompletePolicy is the closed enum for
// `logging.request.incomplete_policy`.
type loggingIncompletePolicy string

const (
	loggingIncompletePolicyWarn     loggingIncompletePolicy = "warn"
	loggingIncompletePolicyFailTest loggingIncompletePolicy = "fail_test"
)

// loggingInventoryMode is the closed enum for `logging.inventory.mode`.
type loggingInventoryMode string

const (
	loggingInventoryModeIndexed loggingInventoryMode = "indexed"
	loggingInventoryModeDeep    loggingInventoryMode = "deep"
)

// loggingDetail is the closed enum for the per-concern `detail` lever.
type loggingDetail string

const (
	loggingDetailUnset   loggingDetail = ""
	loggingDetailSummary loggingDetail = "summary"
	loggingDetailVerbose loggingDetail = "verbose"
)

// loggingLevel is the closed enum for the per-concern `level` lever.
type loggingLevel string

const (
	loggingLevelUnset loggingLevel = ""
	loggingLevelDebug loggingLevel = "debug"
	loggingLevelInfo  loggingLevel = "info"
	loggingLevelWarn  loggingLevel = "warn"
	loggingLevelError loggingLevel = "error"
)
