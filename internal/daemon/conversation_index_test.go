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
