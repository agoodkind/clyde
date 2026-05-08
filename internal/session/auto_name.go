package session

// AutoNameState records where a session sits in the auto-rename
// state machine. The zero value is the untouched state.
type AutoNameState string

const (
	AutoNameStateUntouched  AutoNameState = ""
	AutoNameStateApplied    AutoNameState = "applied"
	AutoNameStateUserLocked AutoNameState = "user_locked"
)

// String returns the canonical wire string for the auto-name state.
// The value matches the JSON and proto string representation so slog
// output and proto round-trips share one vocabulary.
func (s AutoNameState) String() string {
	return string(s)
}

// AutoNameSource records which subsystem produced the current name.
// The zero value means the daemon has not yet attributed a source.
type AutoNameSource string

const (
	AutoNameSourceUnspecified AutoNameSource = ""
	AutoNameSourceDefault     AutoNameSource = "default"
	AutoNameSourceTranscript  AutoNameSource = "transcript"
	AutoNameSourceLLM         AutoNameSource = "llm"
	AutoNameSourceUser        AutoNameSource = "user"
)

// String returns the canonical wire string for the auto-name source.
// The value matches the JSON and proto string representation so slog
// output and proto round-trips share one vocabulary.
func (s AutoNameSource) String() string {
	return string(s)
}
