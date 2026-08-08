package daemon

import (
	"testing"
)

func TestNewConversationRegistryRegistersEveryConversationProvider(t *testing.T) {
	t.Parallel()

	registry := newConversationRegistry()
	providers := ConversationProviders()
	if len(providers) == 0 {
		t.Fatal("conversation provider list is empty")
	}
	for _, provider := range providers {
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
