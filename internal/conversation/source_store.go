package conversation

import (
	"context"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// SourceToolCall contains a native tool call and its source timestamp.
type SourceToolCall struct {
	Tool      transcript.ToolCall
	Timestamp time.Time
	// Reason identifies why the tool call timestamp is unavailable.
	Reason string
}

// SourceMessages contains ordered messages, stored copies, and native tool calls.
type SourceMessages struct {
	Messages []transcript.Message
	// Copies contains stored messages outside the ordered conversation.
	Copies []transcript.Message
	// ToolCalls contains native tool calls in source order.
	ToolCalls []SourceToolCall
	// ToolCallsComplete reports whether the native tool call state is complete.
	ToolCallsComplete bool
}

// SourceStoreReader reads provider messages and prepares stored text for source matching.
type SourceStoreReader interface {
	SourceStoreMessages(ctx context.Context, record Record, opts LoadOptions) (SourceMessages, bool, error)
	// SourceMatchText prepares stored text for matching against source messages.
	SourceMatchText(role string, text string) string
}

// SourceStoreReader returns the provider's source store reader when its parser implements it.
func (r *Registry) SourceStoreReader(provider providerid.Provider) (SourceStoreReader, bool) {
	parser, err := r.Lookup(provider)
	if err != nil {
		return nil, false
	}
	reader, implemented := parser.(SourceStoreReader)
	return reader, implemented
}
