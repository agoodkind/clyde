package daemon

import (
	"testing"

	"goodkind.io/clyde/internal/providerid"
)

func TestNewConversationRegistryRegistersEveryConversationProvider(t *testing.T) {
	t.Parallel()

	registry := newConversationRegistry()
	for _, provider := range []providerid.Provider{
		providerid.ProviderClaude,
		providerid.ProviderCodex,
		providerid.ProviderCursor,
		providerid.ProviderZed,
		providerid.ProviderCopilot,
	} {
		parser, err := registry.Lookup(provider)
		if err != nil {
			t.Fatalf("Lookup(%v) returned error: %v", provider, err)
		}
		if parser == nil {
			t.Fatalf("Lookup(%v) returned nil parser", provider)
		}
		if got := parser.Provider(); got != provider {
			t.Fatalf("parser.Provider() = %v, want %v", got, provider)
		}
	}
}
