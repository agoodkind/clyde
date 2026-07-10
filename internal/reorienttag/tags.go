// Package reorienttag owns the transcript wrapper tags shared by the MITM
// injector and the reorient renderer.
package reorienttag

const (
	// PreCompactionTranscriptOpen starts the recovered transcript block injected
	// into a compact summary.
	PreCompactionTranscriptOpen = "<pre-compaction-transcript>"
	// PreCompactionTranscriptClose closes the recovered transcript block
	// injected into a compact summary.
	PreCompactionTranscriptClose = "</pre-compaction-transcript>"
)
