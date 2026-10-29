package conversation

import "goodkind.io/clyde/internal/providerid"

// StoredTextNormalizer converts stored row text to the text the parser produces
// for a message with the given role.
type StoredTextNormalizer interface {
	NormalizeStoredText(role string, text string) string
}

// StoredTextNormalizer returns the registered parser's normalizer for the
// provider. It returns false if the provider does not have a registered parser or the
// parser does not implement the contract.
func (r *Registry) StoredTextNormalizer(provider providerid.Provider) (StoredTextNormalizer, bool) {
	parser, err := r.Lookup(provider)
	if err != nil {
		return nil, false
	}
	normalizer, implemented := parser.(StoredTextNormalizer)
	return normalizer, implemented
}
