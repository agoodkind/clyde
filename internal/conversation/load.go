package conversation

import (
	"fmt"
	"iter"
	"log/slog"

	"goodkind.io/clyde/internal/transcript"
)

// resolveStream resolves the parser for the record's provider and returns its
// lazy message stream. The caller pulls only what it needs and may stop the
// range early, so a window read never consumes the whole artifact.
func (idx *Index) resolveStream(record Record, opts LoadOptions) (iter.Seq2[transcript.Message, error], error) {
	parser, err := idx.registry.Lookup(record.Provider)
	if err != nil {
		slog.Warn("conversation.load.parser_unresolved", "concern", "conversation.load", "component", "conversation", "provider", record.Provider.String(), "conversation_id", record.ID, "err", err)
		return nil, fmt.Errorf("resolve parser for %s: %w", record.Provider.String(), err)
	}
	return parser.Stream(record.ArtifactPath, opts), nil
}

// LoadMessages reads the whole provider artifact into a slice of generic
// messages. It folds the streaming parse with [CollectMessages] because the
// caller needs every message at once. Window reads should pull the stream
// directly and stop early instead of calling this.
func (idx *Index) LoadMessages(record Record, includeSystemPrompts bool, includeToolOutputs bool) ([]transcript.Message, error) {
	return idx.LoadMessagesWithOptions(record, LoadOptions{
		IncludeSystemPrompts:  includeSystemPrompts,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    includeToolOutputs,
	})
}

// LoadMessagesWithOptions reads the whole provider artifact into a slice of
// generic messages with the supplied streaming options.
func (idx *Index) LoadMessagesWithOptions(record Record, opts LoadOptions) ([]transcript.Message, error) {
	stream, err := idx.resolveStream(record, opts)
	if err != nil {
		return nil, err
	}
	messages, err := CollectMessages(stream)
	if err != nil {
		slog.Warn("conversation.load.collect_failed", "concern", "conversation.load", "component", "conversation", "provider", record.Provider.String(), "conversation_id", record.ID, "err", err)
		return nil, fmt.Errorf("load %s conversation: %w", record.Provider.String(), err)
	}
	return messages, nil
}

const (
	// tailInitialWindowBytes is the first byte window LoadRecentTurns reads,
	// anchored at the artifact's end. Most GetRecentTurns calls resolve
	// within one or two steps because the turn budget is small and
	// qualifying messages tend to be near EOF.
	tailInitialWindowBytes = 256 * 1024
	// tailWindowGrowthFactor is how much each retry widens the window by. A
	// factor of 4 converges to a 1.33x total-bytes-reread tax across steps,
	// versus 2x for doubling, while still reaching a 429MB outlier in seven
	// steps from the 256KiB starting point.
	tailWindowGrowthFactor = 4
)

// LoadRecentTurns returns at least need conversational-turn messages
// (see [IsConversationalTurn]) from the trailing portion of the artifact, in
// the same chronological order and shape [LoadMessagesWithOptions] would
// produce for the same options. When the record's provider parser
// implements [TailParser] and TailSize reports the artifact byte-addressable,
// this reads a bounded window anchored at the artifact's end and grows it
// geometrically until the window holds enough qualifying messages or reaches
// offset 0, instead of decoding the whole artifact. Otherwise, including when
// opts.IncludeToolOutputs is set (StreamFrom cannot support the cross-line
// buffering that needs), it falls back to the full load, so the reply is
// identical either way; only the bytes read differ.
//
// The size used as the window's upper bound is captured once, from a single
// TailSize call, and reused unchanged across every growth step. A file that
// is appended to or truncated between steps would otherwise make growth
// steps disagree with each other (a later, larger window anchored at a later
// EOF is not the same snapshot as an earlier, smaller one); holding the
// upper bound fixed keeps every step reading a consistent snapshot. One
// accepted consequence: the reply reflects the artifact as of that one stat,
// which can be a moment older than what a full read reaches if the file grew
// afterward, and a mid-file read failure past the window is never observed.
// Both make the fast path return a more successful, slightly older answer,
// never a wrong one.
func (idx *Index) LoadRecentTurns(
	record Record,
	need int,
	opts LoadOptions,
) ([]transcript.Message, error) {
	if opts.IncludeToolOutputs || need <= 0 {
		return idx.LoadMessagesWithOptions(record, opts)
	}
	parser, err := idx.registry.Lookup(record.Provider)
	if err != nil {
		slog.Warn("conversation.load.tail_parser_unresolved", "concern", "conversation.load", "component", "conversation", "provider", record.Provider.String(), "conversation_id", record.ID, "err", err)
		return nil, fmt.Errorf("resolve parser for %s: %w", record.Provider.String(), err)
	}
	tailParser, ok := parser.(TailParser)
	if !ok {
		return idx.LoadMessagesWithOptions(record, opts)
	}
	size, ok := tailParser.TailSize(record.ArtifactPath)
	if !ok {
		return idx.LoadMessagesWithOptions(record, opts)
	}

	window := int64(tailInitialWindowBytes)
	for {
		start := max(size-window, 0)
		messages, err := CollectMessages(tailParser.StreamFrom(record.ArtifactPath, opts, start, size))
		if err != nil {
			slog.Warn("conversation.load.tail_read_failed", "concern", "conversation.load", "component", "conversation", "provider", record.Provider.String(), "conversation_id", record.ID, "err", err)
			return nil, fmt.Errorf("tail read %s conversation: %w", record.Provider.String(), err)
		}
		if start == 0 || countConversationalTurns(messages) >= need {
			return messages, nil
		}
		window *= tailWindowGrowthFactor
	}
}

func countConversationalTurns(messages []transcript.Message) int {
	count := 0
	for _, message := range messages {
		if IsConversationalTurn(message) {
			count++
		}
	}
	return count
}
