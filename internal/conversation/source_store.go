package conversation

import (
	"context"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// SourceStoreReader reads provider messages and prepares stored text for source matching.
type SourceStoreReader interface {
	SourceStoreMessages(ctx context.Context, record Record, opts LoadOptions) ([]transcript.Message, bool, error)
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
