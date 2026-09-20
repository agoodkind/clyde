// Package reorienttag owns the wrapper tags shared by the compaction injectors
// and the reorient renderer.
package reorienttag

import "strings"

const (
	// PreCompactionTranscriptOpen starts the recovered transcript block injected
	// into a compact summary.
	PreCompactionTranscriptOpen = "<pre-compaction-transcript>"
	// PreCompactionTranscriptClose closes the recovered transcript block
	// injected into a compact summary.
	PreCompactionTranscriptClose = "</pre-compaction-transcript>"
	// CompactionInstructionsOpen starts the operator instructions block injected
	// after the recovered transcript.
	CompactionInstructionsOpen = "<compaction-instructions>"
	// CompactionInstructionsClose closes the operator instructions block.
	CompactionInstructionsClose = "</compaction-instructions>"
)

// WrapInjection builds the text every compaction injector inserts into a
// summary: the transcript between PreCompactionTranscriptOpen and
// PreCompactionTranscriptClose, then, when instructions is non-empty, the
// instructions between CompactionInstructionsOpen and
// CompactionInstructionsClose.
//
// The leading blank line and the trailing newline are part of the contract.
// The Codex JSON append compares the whole wrapped text against a stored
// summary to decide whether an injection already happened.
func WrapInjection(transcript, instructions string) string {
	var builder strings.Builder
	builder.WriteString("\n\n")
	builder.WriteString(PreCompactionTranscriptOpen)
	builder.WriteByte('\n')
	builder.WriteString(transcript)
	builder.WriteByte('\n')
	builder.WriteString(PreCompactionTranscriptClose)
	builder.WriteByte('\n')
	if instructions == "" {
		return builder.String()
	}
	builder.WriteByte('\n')
	builder.WriteString(CompactionInstructionsOpen)
	builder.WriteByte('\n')
	builder.WriteString(instructions)
	builder.WriteByte('\n')
	builder.WriteString(CompactionInstructionsClose)
	builder.WriteByte('\n')
	return builder.String()
}
