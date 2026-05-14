package compact

import (
	"time"

	"goodkind.io/clyde/internal/session"
)

// RuntimeUsageCategory mirrors a single /context category row for the
// metrics dashboard. The runtime layer owns this provider-neutral
// shape so the daemon can transport per-row data on the upfront
// snapshot without the dashboard re-running the probe.
type RuntimeUsageCategory struct {
	Name       string
	Tokens     int
	IsDeferred bool
	// Color is the provider-supplied color hint resolved through the
	// categorystyle registry at runtime. Empty string means no hint
	// was registered; consumers fall back to their default palette.
	Color string
}

const DefaultCountModel = "claude-sonnet-4-5"

type RuntimeMode int

const (
	RuntimeModePreview RuntimeMode = iota
	RuntimeModeApply
)

type RuntimeRequest struct {
	Session       *session.Session
	Store         session.Store
	TargetTokens  int
	Reserved      int
	Model         string
	ModelExplicit bool
	Strippers     Strippers
	Summarize     bool
	SummarizeMode SummarizeMode
	Force         bool
	Mode          RuntimeMode

	// Refresh asks the upfront builder to bypass any cached
	// context-usage snapshot and force a fresh provider-native probe.
	// Wired from `clyde compact --refresh` through the CompactPreview
	// RPC; defaults to false for every other caller.
	Refresh bool

	PreparedUpfront        *RuntimeUpfront
	PreparedStaticOverhead int
	PreparedSlice          *Slice
}

type RuntimeUpfront struct {
	SessionName         string
	SessionID           string
	Model               string
	CurrentTotal        int
	MaxTokens           int
	Messages            int
	CompactBuffer       int
	Free                int
	ContextOverhead     int
	Target              int
	StaticFloor         int
	Reserved            int
	Thinking            int
	Images              int
	ToolPairs           int
	ChatTurns           int
	StrippersText       string
	TargetDate          string
	PostBoundaryEntries int
	Calibrated          bool
	CalibrationOverhead int

	// Metrics-dashboard supplemental fields. Populated by
	// BuildRuntimeUpfront so the daemon can serve the metrics-only
	// dashboard without the CLI re-reading the transcript or
	// re-probing /context.
	TranscriptPath  string
	FileSizeBytes   int64
	FileLineCount   int
	HasBoundary     bool
	BoundaryLine    int
	BoundaryUUID    string
	BoundaryTime    time.Time
	UsagePercentage int
	UsageAvailable  bool
	UsageSource     string
	UsageCapturedAt time.Time
	UsageError      string
	UsageCategories []RuntimeUsageCategory
}

type RuntimeIteration struct {
	Iteration IterationRecord
	Accepted  bool
}

type RuntimeResult struct {
	Upfront        RuntimeUpfront
	ModelForCount  string
	ModelForRender string
	Slice          *Slice
	Plan           *PlanResult
	Apply          *ApplyResult
	TranscriptPath string
}
