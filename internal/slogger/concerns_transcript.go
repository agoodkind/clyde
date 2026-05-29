package slogger

// Transcript concern constants. The transcript reader shared by CLI
// and MCP surfaces emits read/marshal/parse failures from
// `internal/transcript/`.
const (
	ConcernTranscript = "transcript"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernTranscript: "transcript/transcript.jsonl",
	})
}
