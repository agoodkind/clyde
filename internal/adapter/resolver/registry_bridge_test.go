package resolver

import (
	"testing"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	"goodkind.io/clyde/internal/config"
)

func TestBackendToProviderMapping(t *testing.T) {
	cases := []struct {
		backend string
		want    ProviderID
	}{
		{adaptermodel.BackendAnthropic, ProviderAnthropic},
		{adaptermodel.BackendCodex, ProviderCodex},
		{adaptermodel.BackendClaude, ProviderUnknown},
		{adaptermodel.BackendPassthroughOverride, ProviderUnknown},
		{"", ProviderUnknown},
		{"nonsense", ProviderUnknown},
	}
	for _, tc := range cases {
		if got := backendToProvider(tc.backend); got != tc.want {
			t.Errorf("backendToProvider(%q) = %v, want %v", tc.backend, got, tc.want)
		}
	}
}

func TestModelRegistryAdapterNilInner(t *testing.T) {
	a := NewModelRegistryAdapter(nil)
	if _, err := a.Resolve("anything", ""); err == nil {
		t.Fatal("expected error from nil-inner adapter, got nil")
	}
	var nilAdapter *ModelRegistryAdapter
	if _, err := nilAdapter.Resolve("anything", ""); err == nil {
		t.Fatal("expected error from nil adapter, got nil")
	}
}

func TestModelRegistryAdapterPreservesInstructions(t *testing.T) {
	cfg := config.AdapterConfig{
		DefaultModel: "clyde-haiku-4.5",
		ClientIdentity: config.AdapterClientIdentity{
			SystemPromptPrefix:      "prefix",
			StainlessPackageVersion: "test",
			StainlessRuntime:        "go",
			StainlessRuntimeVersion: "1.0",
			CCVersion:               "1.0.0",
			CCEntrypoint:            "test",
		},
		Families: map[string]config.AdapterFamily{
			"haiku-4-5": {
				AliasPrefix:     "haiku-4.5",
				Model:           "claude-haiku-4-5-20251001",
				Efforts:         []string{"medium"},
				ThinkingModes:   []string{"default"},
				MaxOutputTokens: 16000,
				SupportsTools:   new(true),
				SupportsVision:  new(true),
				Contexts: []config.AdapterModelContext{{
					Tokens: 200000,
				}},
			},
		},
		Models: map[string]config.AdapterModel{
			"custom-model": {
				Backend:      adaptermodel.BackendAnthropic,
				Model:        "claude-custom",
				Instructions: "follow repo conventions",
				Context:      200000,
				Efforts:      []string{"medium"},
			},
		},
	}
	registry, err := adaptermodel.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	adapter := NewModelRegistryAdapter(registry)
	got, err := adapter.Resolve("custom-model", "medium")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Instructions != "follow repo conventions" {
		t.Fatalf("Instructions = %q want %q", got.Instructions, "follow repo conventions")
	}
}
