package session

// AutoNameState records where a session sits in the auto-rename
// state machine. The zero value is the untouched state.
type AutoNameState string

const (
	// AutoNameStateUntouched marks a session that has not yet been
	// considered by the auto-rename state machine. It is the zero
	// value and matches sessions written before the field existed.
	AutoNameStateUntouched AutoNameState = ""
	// AutoNameStateApplied marks a session whose current name was
	// produced by the auto-rename pipeline.
	AutoNameStateApplied AutoNameState = "applied"
	// AutoNameStateUserLocked marks a session whose name has been
	// edited by the user, which suppresses further auto-rename runs.
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
	// AutoNameSourceUnspecified means the daemon has not attributed a
	// source for the current name. It is the zero value.
	AutoNameSourceUnspecified AutoNameSource = ""
	// AutoNameSourceDefault marks a name produced by the default
	// fallback path when no richer source was available.
	AutoNameSourceDefault AutoNameSource = "default"
	// AutoNameSourceTranscript marks a name derived from transcript
	// scanning heuristics rather than an LLM.
	AutoNameSourceTranscript AutoNameSource = "transcript"
	// AutoNameSourceLLM marks a name produced by the auto-rename LLM
	// summarization step.
	AutoNameSourceLLM AutoNameSource = "llm"
	// AutoNameSourceUser marks a name supplied directly by the user,
	// which the auto-rename pipeline must not overwrite.
	AutoNameSourceUser AutoNameSource = "user"
)

// String returns the canonical wire string for the auto-name source.
// The value matches the JSON and proto string representation so slog
// output and proto round-trips share one vocabulary.
func (s AutoNameSource) String() string {
	return string(s)
}
