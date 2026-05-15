package compact

import (
	"time"

	"goodkind.io/clyde/internal/session"
)

// RuntimeUsageCategory mirrors a single context-usage category row for
// the metrics dashboard. The runtime layer owns this provider-neutral
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

// DefaultCountModel is the fallback context-count model for compact runtime runs.
const DefaultCountModel = "claude-sonnet-4-5"

// RuntimeMode selects whether a runtime compact run previews or applies changes.
type RuntimeMode int

const (
	// RuntimeModePreview runs compaction planning without mutating the transcript.
	RuntimeModePreview RuntimeMode = iota
	// RuntimeModeApply runs compaction planning and applies the transcript mutation.
	RuntimeModeApply
)

// RuntimeRequest is the full input bundle for daemon-backed compaction.
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

// RuntimeUpfront contains transcript and context metadata gathered before planning.
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

	// Upfront supplemental fields. Populated by BuildRuntimeUpfront so
	// clients can display transcript, boundary, and context details
	// without re-reading the transcript or re-running the context usage
	// probe.
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

// RuntimeIteration reports one planner iteration to runtime stream consumers.
type RuntimeIteration struct {
	Iteration IterationRecord
	Accepted  bool
}

// RuntimeResult is the complete output from a runtime compact run.
type RuntimeResult struct {
	Upfront        RuntimeUpfront
	ModelForCount  string
	ModelForRender string
	Slice          *Slice
	Plan           *PlanResult
	Apply          *ApplyResult
	TranscriptPath string
}
