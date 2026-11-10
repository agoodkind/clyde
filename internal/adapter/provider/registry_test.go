package provider

import (
	"context"
	"errors"
	"sort"
	"testing"

	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

type stubProvider struct {
	id adapterresolver.ProviderID
}

func (s stubProvider) ID() adapterresolver.ProviderID { return s.id }
func (s stubProvider) Execute(_ context.Context, _ adapterresolver.ResolvedRequest, _ EventWriter) (Result, error) {
	return Result{}, nil
}

func TestRegistryLookupMissingReturnsTypedError(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Lookup(adapterresolver.ProviderCodex); !errors.Is(err, ErrProviderNotRegistered) {
		t.Fatalf("expected ErrProviderNotRegistered, got %v", err)
	}
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	want := stubProvider{id: adapterresolver.ProviderCodex}
	r.Register(want)
	got, err := r.Lookup(adapterresolver.ProviderCodex)
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if got.ID() != want.ID() {
		t.Errorf("provider ID = %v, want %v", got.ID(), want.ID())
	}
}

func TestRegistryRegisterRejectsInvalidProvider(t *testing.T) {
	r := NewRegistry()
	r.Register(nil)
	r.Register(stubProvider{id: adapterresolver.ProviderUnknown})
	r.Register(stubProvider{id: adapterresolver.ProviderID("not-a-real-backend")})
	if ids := r.IDs(); len(ids) != 0 {
		t.Errorf("expected empty registry, got %v", ids)
	}
}

func TestRegistryIDs(t *testing.T) {
	r := NewRegistry()
	r.Register(stubProvider{id: adapterresolver.ProviderCodex})
	r.Register(stubProvider{id: adapterresolver.ProviderAnthropic})
	ids := r.IDs()
	stringsList := make([]string, 0, len(ids))
	for _, id := range ids {
		stringsList = append(stringsList, id.String())
	}
	sort.Strings(stringsList)
	if len(stringsList) != 2 || stringsList[0] != "anthropic" || stringsList[1] != "codex" {
		t.Errorf("unexpected IDs: %v", stringsList)
	}
}
