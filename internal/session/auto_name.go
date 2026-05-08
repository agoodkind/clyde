package session

// AutoNameState records where a session sits in the auto-rename
// state machine. The zero value is the untouched state.
type AutoNameState string

// Auto-rename state machine values. The daemon picks one of these per
// session and propagates it through the proto so the TUI can render
// the badge or sort accordingly without re-deriving state.
const (
	// AutoNameStateUntouched is the zero value. The session has not
	// yet entered the auto-rename pipeline.
	AutoNameStateUntouched AutoNameState = ""
	// AutoNameStateApplied means the daemon ran the auto pass and
	// the resulting candidate landed on the session name.
	AutoNameStateApplied AutoNameState = "applied"
	// AutoNameStateUserLocked means the user typed a name through
	// the rename modal. The auto worker excludes the session forever.
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

// Auto-rename source attribution. Records which subsystem produced
// the current session name so the worker can decide whether to
// re-evaluate or skip the session.
const (
	// AutoNameSourceUnspecified is the zero value. The daemon has
	// not yet attributed a source.
	AutoNameSourceUnspecified AutoNameSource = ""
	// AutoNameSourceDefault means the session still carries the
	// generated default-shaped placeholder name.
	AutoNameSourceDefault AutoNameSource = "default"
	// AutoNameSourceTranscript means the auto worker derived the
	// name from the transcript without an LLM call.
	AutoNameSourceTranscript AutoNameSource = "transcript"
	// AutoNameSourceLLM means the auto worker derived the name from
	// an LLM call against the configured adapter route.
	AutoNameSourceLLM AutoNameSource = "llm"
	// AutoNameSourceUser means the user typed the name through the
	// rename modal. The auto worker treats this as locked.
	AutoNameSourceUser AutoNameSource = "user"
)

// String returns the canonical wire string for the auto-name source.
// The value matches the JSON and proto string representation so slog
// output and proto round-trips share one vocabulary.
func (s AutoNameSource) String() string {
	return string(s)
}
