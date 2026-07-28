package conversation

import (
	"context"
	"fmt"
	"iter"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// FileStamp captures a transcript file's size and modification time. A scan
// reuses the previously parsed record for any artifact whose stamp is
// unchanged, so the steady state stats files instead of re-reading them.
type FileStamp struct {
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
}

// Equal reports whether two stamps describe the same on-disk file state.
func (a FileStamp) Equal(b FileStamp) bool {
	return a.Size == b.Size && a.Mtime.Equal(b.Mtime)
}

// Fingerprint renders the stamp as a stable content fingerprint string for the
// engine's conversation manifest. It changes whenever the file's size or
// modification time changes, which for an append-only transcript is exactly when
// its content grows, so the engine re-embeds a conversation only when it changes.
func (a FileStamp) Fingerprint() string {
	return fmt.Sprintf("%d:%d", a.Size, a.Mtime.UnixNano())
}

// readerGeneration counts the times a provider's reader has changed what it
// produces from bytes that did not change.
//
// A conversation's message index is a position the search path feeds back into
// the loader, so a reader change that renumbers messages leaves every stored
// index pointing at a different turn. The artifact itself does not change, so
// the file stamp cannot notice, and a finished transcript would never be re-fed.
//
// Including the generation in the advertised fingerprint re-advertises every
// conversation of that provider exactly once. The engine then asks for those
// conversations through the ordinary additive sync and clyde delivers their
// documents again. Nothing here rewrites or deletes a stored row, so this is a
// re-feed rather than a backfill.
//
// It is keyed by artifact kind rather than by provider, because a provider can
// read several unrelated stores and only one of them may have changed. Bump an
// artifact kind's entry when its reader changes which messages it yields, or
// their positions, for an unchanged artifact. A kind absent from the map is at
// generation zero and keeps the bare stamp fingerprint, so a store whose reader
// did not change is never re-embedded.
var readerGeneration = map[ArtifactKind]int{
	// Cursor turn reconstruction groups each turn's records into one message,
	// which renumbers every message after the first multi-record turn. It changed
	// the JSONL transcript reader alone: Cursor's composer and legacy-chat stores
	// have their own readers and their numbering is untouched.
	ArtifactKindCursorAgentTranscript: 1,
}

// ArtifactKind names the store an artifact came from, which decides both which
// reader produced its messages and, through [readerGeneration], whether that
// reader has changed what it yields.
//
// [Record.ArtifactKind] is still a plain string because it crosses the daemon's
// wire format, where changing it reaches every provider and both RPC surfaces.
// Converting at the one lookup below keeps the classification typed everywhere
// this package owns it.
type ArtifactKind string

// ArtifactKindCursorAgentTranscript names Cursor's modern JSONL transcript. It
// is declared here because [readerGeneration] keys on it and this package cannot
// import a provider package.
const ArtifactKindCursorAgentTranscript ArtifactKind = "cursor_agent_transcript"

// ContentFingerprint is the value a conversation manifest advertises for one
// conversation. It changes when the artifact changes, and when the reader for
// that artifact kind changes what it produces from an unchanged artifact.
//
// Every caller that builds a manifest uses this, so the daemon sync and the
// operator backfill state the same value for the same conversation. Two
// implementations would report every conversation as needed on each alternation
// between them, re-embedding the corpus once per run.
func ContentFingerprint(record Record, stamp FileStamp) string {
	generation := readerGeneration[ArtifactKind(record.ArtifactKind)]
	if generation == 0 {
		return stamp.Fingerprint()
	}
	return fmt.Sprintf("%s:r%d", stamp.Fingerprint(), generation)
}

// ScanCandidate is one artifact a provider's [Parser.Discover] surfaced for the
// incremental scan. Stamp lets the scan driver skip files whose size and mtime
// are unchanged, reusing the prior record without re-reading the file.
type ScanCandidate struct {
	Path  string
	Stamp FileStamp
}

// LoadOptions configures how [Parser.Stream] reads a single artifact.
type LoadOptions struct {
	IncludeSystemPrompts  bool
	IncludeSystemMessages bool
	IncludeToolOutputs    bool
}

// Parser reads one provider's raw conversation artifacts. Each provider
// implements this interface in its own package and registers it in a [Registry]
// at daemon and CLI startup. The conversation package never imports a provider
// package, so the only coupling is through this interface.
type Parser interface {
	// Provider returns the provider identity this parser handles.
	Provider() providerid.Provider
	// Discover finds the provider's artifacts. prior is the previous scan's
	// records keyed by artifact path, which a provider whose discovery stats
	// files itself (Codex) may consult; a provider that walks a directory
	// (Claude) may ignore it.
	Discover(ctx context.Context, prior map[string]Record) ([]ScanCandidate, error)
	// ScanRecord reads only the header of one artifact and stops early once it
	// has the id, title, and created time. It never reads to EOF. The second
	// return is false when the artifact yields no usable record.
	ScanRecord(path string, stamp FileStamp) (Record, bool)
	// Stream lazily yields one message at a time over the whole artifact. The
	// implementation holds at most one message in flight; only [CollectMessages]
	// builds a slice. A caller may stop the range early to read a window.
	Stream(path string, opts LoadOptions) iter.Seq2[transcript.Message, error]
}

// CollectMessages folds a streaming parse into a full slice. Use it only where a
// caller needs the whole conversation at once, such as export or full render.
func CollectMessages(stream iter.Seq2[transcript.Message, error]) ([]transcript.Message, error) {
	var out []transcript.Message
	for message, err := range stream {
		if err != nil {
			return out, err
		}
		out = append(out, message)
	}
	return out, nil
}
