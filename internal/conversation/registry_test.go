package conversation

import (
	"context"
	"errors"
	"iter"
	"testing"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// fakeParser is a minimal [Parser] for registry tests. It declares a provider
// and returns nothing, so the conversation package needs no provider import to
// exercise registration and lookup.
type fakeParser struct {
	provider providerid.Provider
}

func (f fakeParser) Provider() providerid.Provider { return f.provider }

func (fakeParser) Discover(context.Context, map[string]Record) ([]ScanCandidate, error) {
	return nil, nil
}

func (fakeParser) ScanRecord(string, FileStamp) (Record, bool) {
	return Record{}, false
}

func (fakeParser) Stream(string, LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(func(transcript.Message, error) bool) {}
}

func TestRegistryResolvesRegisteredProvidersAndRejectsUnknown(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	registry.Register(fakeParser{provider: providerid.ProviderClaude})
	registry.Register(fakeParser{provider: providerid.ProviderCodex})

	if _, err := registry.Lookup(providerid.ProviderClaude); err != nil {
		t.Fatalf("lookup claude: %v", err)
	}
	if _, err := registry.Lookup(providerid.ProviderCodex); err != nil {
		t.Fatalf("lookup codex: %v", err)
	}
	if _, err := registry.Lookup(providerid.ProviderCursor); !errors.Is(err, ErrProviderNotRegistered) {
		t.Fatalf("lookup unregistered provider err = %v, want ErrProviderNotRegistered", err)
	}
}

func TestRegistryIgnoresNilAndInvalidProvider(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	registry.Register(nil)
	registry.Register(fakeParser{provider: providerid.ProviderUnspecified})
	if got := len(registry.Providers()); got != 0 {
		t.Fatalf("registered providers = %d, want 0", got)
	}
}
