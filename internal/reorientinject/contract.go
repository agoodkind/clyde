package reorientinject

// SegmentKind is the content kind of one countable piece of a compaction
// request. The split uses it to skip a kind the export arguments exclude.
type SegmentKind uint8

const (
	// KindText is an assistant or user text block.
	KindText SegmentKind = iota
	// KindThinking is model reasoning.
	KindThinking
	// KindToolUse is a tool call.
	KindToolUse
	// KindToolResult is the answer to a tool call.
	KindToolResult
	// KindImage is an image block.
	KindImage
	// KindOther is a block kind the provider does not classify.
	KindOther
)

// Segment is one content block. Text is what the token counter measures and
// what the injection includes.
type Segment struct {
	Kind SegmentKind
	Text string
}

// Message is one message of a compaction request.
type Message struct {
	Role     string
	Segments []Segment
}

// ParsedRequest is an intercepted compaction request, decoded by a provider.
type ParsedRequest struct {
	// SessionID identifies the conversation in the provider's own terms.
	SessionID string
	Messages  []Message
	// InstructionStart is the index of the compaction prompt. The messages from
	// that index onward stay in the forwarded request unmodified.
	InstructionStart int
	// Arguments are the export arguments the operator typed after the
	// compaction command, in command-line form.
	Arguments []string
}

// Cut is where the split divides the boundary message. HeadRunes is how much of
// the boundary segment's text the model summarizes.
type Cut struct {
	MessageIndex int
	SegmentIndex int
	HeadRunes    int
}

// Provider adapts one wire protocol to the split. Implementations own every
// wire shape; this package names none of them.
type Provider interface {
	// ParseCompaction decodes an intercepted request. It returns ok=false for a
	// body that is not a compaction request, which leaves the request alone.
	ParseCompaction(body []byte) (ParsedRequest, bool)
	// Truncate rewrites body to end the conversation at the cut, keeping the
	// instruction region from instructionStart onward.
	Truncate(body []byte, cut Cut, instructionStart int) ([]byte, error)
	// InjectSummary inserts content into the summary the model returned.
	InjectSummary(body []byte, content string) ([]byte, error)
}
