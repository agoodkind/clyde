package daemon

import (
	"testing"

	"goodkind.io/clyde/internal/providerid"
)

func TestNewConversationRegistryRegistersZedParser(t *testing.T) {
	t.Parallel()

	registry := newConversationRegistry()
	parser, err := registry.Lookup(providerid.ProviderZed)
	if err != nil {
		t.Fatalf("Lookup(ProviderZed) returned error: %v", err)
	}
	if parser == nil {
		t.Fatal("Lookup(ProviderZed) returned nil parser")
	}
}

func TestNewConversationRegistryRegistersCursorParser(t *testing.T) {
	t.Parallel()

	registry := newConversationRegistry()
	parser, err := registry.Lookup(providerid.ProviderCursor)
	if err != nil {
		t.Fatalf("Lookup(ProviderCursor) returned error: %v", err)
	}
	if parser == nil {
		t.Fatal("Lookup(ProviderCursor) returned nil parser")
	}
	if got := parser.Provider(); got != providerid.ProviderCursor {
		t.Fatalf("cursor parser Provider() = %v, want %v", got, providerid.ProviderCursor)
	}
}

func TestNewConversationRegistryRegistersEveryConversationProvider(t *testing.T) {
	t.Parallel()

	registry := newConversationRegistry()
	for _, provider := range []providerid.Provider{
		providerid.ProviderClaude,
		providerid.ProviderCodex,
		providerid.ProviderCursor,
		providerid.ProviderZed,
	} {
		parser, err := registry.Lookup(provider)
		if err != nil {
			t.Fatalf("Lookup(%v) returned error: %v", provider, err)
		}
		if parser == nil {
			t.Fatalf("Lookup(%v) returned nil parser", provider)
		}
	}
}
