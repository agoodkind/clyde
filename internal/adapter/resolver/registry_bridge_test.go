package resolver

import (
	"testing"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	"goodkind.io/clyde/internal/config"
)

func TestProviderIDAliasesBackendID(t *testing.T) {
	cases := []struct {
		backend adaptermodel.BackendID
		want    ProviderID
	}{
		{adaptermodel.BackendAnthropic, ProviderAnthropic},
		{adaptermodel.BackendCodex, ProviderCodex},
		{adaptermodel.BackendPassthroughOverride, ProviderPassthrough},
		{adaptermodel.BackendID(""), ProviderUnknown},
	}
	for _, tc := range cases {
		if tc.backend != tc.want {
			t.Errorf("backend %q != provider constant %q", tc.backend, tc.want)
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
				Backend:      adaptermodel.BackendAnthropic.String(),
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

func TestModelRegistryAdapterProjectsCapabilityFields(t *testing.T) {
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
				ThinkingModes:   []string{"default", "enabled"},
				MaxOutputTokens: 16000,
				SupportsTools:   new(true),
				SupportsVision:  new(false),
				Contexts: []config.AdapterModelContext{{
					Tokens:         200000,
					ObservedTokens: 272000,
				}},
			},
		},
	}
	registry, err := adaptermodel.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	adapter := NewModelRegistryAdapter(registry)
	got, err := adapter.Resolve("clyde-haiku-4.5-medium", "medium")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Alias != "clyde-haiku-4.5-medium" {
		t.Errorf("Alias = %q, want clyde-haiku-4.5-medium", got.Alias)
	}
	if !got.SupportsTools {
		t.Errorf("SupportsTools = %v, want true", got.SupportsTools)
	}
	if got.SupportsVision {
		t.Errorf("SupportsVision = %v, want false", got.SupportsVision)
	}
	if got.ObservedContext != 272000 {
		t.Errorf("ObservedContext = %d, want 272000", got.ObservedContext)
	}
	if len(got.ThinkingModes) != 2 || got.ThinkingModes[0] != "default" || got.ThinkingModes[1] != "enabled" {
		t.Errorf("ThinkingModes = %v, want [default enabled]", got.ThinkingModes)
	}
}

func TestModelRegistryAdapterProjectsPassthroughOverride(t *testing.T) {
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
		PassthroughOverrides: map[string]config.AdapterPassthroughOverride{
			"vendor-x": {
				BaseURL:   "https://upstream.invalid/v1",
				APIKeyEnv: "UPSTREAM_API_KEY",
				Model:     "upstream-model",
			},
		},
		Models: map[string]config.AdapterModel{
			"vendor-alias": {
				Backend:             adaptermodel.BackendPassthroughOverride.String(),
				PassthroughOverride: "vendor-x",
			},
		},
	}
	registry, err := adaptermodel.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	adapter := NewModelRegistryAdapter(registry)
	got, err := adapter.Resolve("vendor-alias", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Provider != ProviderPassthrough {
		t.Errorf("Provider = %v, want %v", got.Provider, ProviderPassthrough)
	}
	if got.PassthroughOverrideName != "vendor-x" {
		t.Errorf("PassthroughOverrideName = %q, want vendor-x", got.PassthroughOverrideName)
	}
}
