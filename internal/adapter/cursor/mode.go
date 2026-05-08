package cursor

// Mode names a Cursor product mode the request was issued under. The string
// value is the wire form Cursor uses on its public APIs.
type Mode string

const (
	// ModeAgent is the default Cursor mode that may invoke any tool.
	ModeAgent Mode = "agent"
	// ModePlan is the Cursor planning mode that emits markdown-only output.
	ModePlan Mode = "plan"
)
