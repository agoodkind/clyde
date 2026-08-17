package conversation

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"runtime/debug"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/slogger"
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

// ArtifactKind names the store an artifact came from.
//
// [Record.ArtifactKind] is still a plain string because it crosses the daemon's
// wire format, where changing it reaches every provider and both RPC surfaces.
// Converting at the one lookup below keeps the classification typed everywhere
// this package owns it.
type ArtifactKind string

// ArtifactKindCursorAgentTranscript names Cursor's modern JSONL transcript. It
// is declared here so generic fingerprinting can recognize it without importing
// a provider package.
const ArtifactKindCursorAgentTranscript ArtifactKind = "cursor_agent_transcript"

// cursorAgentTranscriptCompatibilitySuffix preserves the fingerprint bytes
// already advertised after Cursor turn reconstruction changed. It is fixed so a
// later parser change cannot trigger an automatic corpus re-feed.
const cursorAgentTranscriptCompatibilitySuffix = ":r1"

// ContentFingerprint is the value a conversation manifest advertises for one
// conversation. It changes only when the artifact stamp changes. Cursor's JSONL
// transcript keeps its historical compatibility suffix without making parser
// revisions part of the fingerprint contract.
//
// Every caller that builds a manifest uses this, so the daemon sync and the
// operator backfill state the same value for the same conversation. Two
// implementations would report every conversation as needed on each alternation
// between them, re-embedding the corpus once per run.
func ContentFingerprint(record Record, stamp FileStamp) string {
	if ArtifactKind(record.ArtifactKind) == ArtifactKindCursorAgentTranscript {
		return stamp.Fingerprint() + cursorAgentTranscriptCompatibilitySuffix
	}
	return stamp.Fingerprint()
}

// ScanCandidate is one artifact a provider's [Parser.Discover] surfaced for the
// incremental scan. Stamp lets the scan driver skip files whose size and mtime
// are unchanged, reusing the prior record without re-reading the file.
type ScanCandidate struct {
	Path     string
	Selector string
	Stamp    FileStamp
}

// MultiConversationScan is one incremental read of an artifact that can hold
// several independently selected conversations. StartOffset is zero for a full
// scan and otherwise points at the first byte after the last complete record
// from the prior scan. PriorRecords contains the records produced before that
// offset so the parser can return the complete current record set.
type MultiConversationScan struct {
	Candidate    ScanCandidate
	PriorRecords []Record
	StartOffset  int64
}

// MultiConversationScanResult is the complete current record set for one
// physical artifact and the byte boundary through its last complete record.
type MultiConversationScanResult struct {
	Records        []Record
	CompleteOffset int64
}

// MultiConversationScanState is the persisted position of the last successful
// multi-conversation artifact scan.
type MultiConversationScanState struct {
	Stamp          FileStamp `json:"stamp"`
	CompleteOffset int64     `json:"complete_offset"`
}

// LoadOptions configures how [Parser.Stream] reads a single artifact.
type LoadOptions struct {
	IncludeSystemPrompts  bool
	IncludeSystemMessages bool
	IncludeToolOutputs    bool
	// IncludeInjected keeps text that hooks and user tooling pushed into user
	// messages. When false, each provider parser removes the injected content
	// it recognizes; the markers stay provider-owned behind this generic bit.
	IncludeInjected bool
	// HarnessTally, when non-nil, accumulates what the parser removed or
	// withheld under the options above: injected content and harness system
	// content. It counts at the point of removal, so a record dropped entirely
	// because stripping emptied it, or withheld whole by an exclusion gate,
	// still counts. Nil disables tallying.
	HarnessTally *transcript.HarnessStrips
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

// MultiConversationParser is implemented by providers whose one physical
// artifact contains several independently addressable conversations.
type MultiConversationParser interface {
	Parser
	ScanRecords(input MultiConversationScan) (MultiConversationScanResult, bool)
	StreamSelected(path string, selector string, opts LoadOptions) iter.Seq2[transcript.Message, error]
}

// CollectMessages folds a streaming parse into a full slice. Use it only where a
// caller needs the whole conversation at once, such as export or full render.
//
// A provider defect that panics part way through an artifact becomes an error
// here rather than unwinding into the caller, so a defect in one reader cannot
// take down whichever surface was reading. The messages collected before the
// panic are returned alongside the error, matching how a stream error already
// reports. Callers that treat any error as fatal, which is all of them today,
// discard them.
//
// Every caller that needs a whole conversation folds through here, so one
// recover covers export, search, and the ingestion feeder. A caller
// that ranges the parser stream itself, rather than folding it, is not covered.
//
// This is the outer bound, not the working one. A push iterator dies where it
// panics and cannot be resumed, so recovering here loses the rest of the
// artifact. [transcript.ContainMappingPanic] sits inside each provider's
// per-record mapping function and costs one record instead, and it is what
// handles a mapping defect in practice. This catches what panics between those
// functions: the read loop, the decode, and the turn assembly.
func CollectMessages(stream iter.Seq2[transcript.Message, error]) (collected []transcript.Message, err error) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		err = fmt.Errorf("collect conversation messages: parser panicked: %v", recovered)
		slog.Error("conversation.collect_panic",
			"concern", slogger.ConcernConversationLoad,
			"component", "conversation",
			"collected", len(collected),
			"err", err,
			"stack", string(debug.Stack()),
		)
	}()
	for message, streamErr := range stream {
		if streamErr != nil {
			return collected, streamErr
		}
		collected = append(collected, message)
	}
	return collected, nil
}
