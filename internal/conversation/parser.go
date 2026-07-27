package conversation

import (
	"context"
	"fmt"
	"iter"
	"strings"
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

// TailParser is implemented by a provider whose artifact is a byte-addressable,
// line-oriented file: [Parser.Stream]'s forward per-line decode has no state
// carried across lines under the caller's options, so a caller that only
// needs the newest few messages, such as [contextsvc], can read a bounded
// byte range near the end instead of the whole file. A registered [Parser]
// is type-asserted against this interface; a provider that does not
// implement it, or whose [TailParser.TailSize] reports an artifact is not
// byte-addressable, falls back to [Parser.Stream] plus [CollectMessages].
//
// One [Parser] can serve more than one artifact kind (Cursor serves a JSONL
// kind plus two SQLite-backed kinds behind one parser), so the capability
// check is keyed to the artifact path, not to the provider as a whole:
// TailSize itself decides per artifact whether a bounded read applies.
type TailParser interface {
	Parser
	// TailSize reports the artifact's current byte size and whether this
	// artifact is byte-addressable this way. ok is false when the artifact
	// is not line-oriented on disk (for example, a provider that unmarshals
	// its whole document from a SQLite blob before yielding anything).
	TailSize(path string) (size int64, ok bool)
	// StreamFrom yields exactly what Stream would yield, restricted to the
	// byte range [start, end), beginning at the first full line at or after
	// start. start == 0 begins at the first byte with no discard, so
	// StreamFrom(path, opts, 0, size) is byte-identical to Stream(path, opts)
	// for the same size. It returns an error rather than a partial result
	// when opts require a full forward read (IncludeToolOutputs needs
	// cross-line buffering to attach a tool result to an earlier call, which
	// a bounded suffix cannot do).
	StreamFrom(path string, opts LoadOptions, start int64, end int64) iter.Seq2[transcript.Message, error]
}

// IsConversationalTurn reports whether a message counts as a visible
// conversation turn: role user or assistant. [Index.LoadRecentTurns]'s
// growth-loop counter and [contextsvc]'s reply-shaping filter both call this
// one function, so there is structurally one predicate for what qualifies,
// not two definitions that could drift apart.
func IsConversationalTurn(message transcript.Message) bool {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	return role == "user" || role == "assistant"
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
