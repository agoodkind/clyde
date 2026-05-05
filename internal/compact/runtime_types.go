package compact

import "goodkind.io/clyde/internal/session"

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

	PreparedUpfront        *RuntimeUpfront
	PreparedStaticOverhead int
	PreparedSlice          *Slice
}

type RuntimeUpfront struct {
	SessionName   string
	SessionID     string
	Model         string
	CurrentTotal  int
	MaxTokens     int
	Target        int
	StaticFloor   int
	Reserved      int
	Thinking      int
	Images        int
	ToolPairs     int
	ChatTurns     int
	StrippersText string
	TargetDate    string
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
