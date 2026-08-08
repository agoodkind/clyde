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
	if multi, ok := parser.(MultiConversationParser); ok {
		return multi.StreamSelected(record.ArtifactPath, record.Selector, opts), nil
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
		// The legacy two-flag signature predates the injected kind; its callers
		// read conversations for people or models, where hook-pushed context is
		// noise, so injected content stays stripped.
		IncludeInjected: false,
		HarnessTally:    nil,
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
