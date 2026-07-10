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
	for _, test := range cases {
		if test.backend != test.want {
			t.Errorf("backend %q != provider constant %q", test.backend, test.want)
		}
	}
}

func TestModelRegistryAdapterNilInner(t *testing.T) {
	adapter := NewModelRegistryAdapter(nil)
	if _, err := adapter.Resolve(IngressOpenAI, "anything", ""); err == nil {
		t.Fatal("expected error from nil-inner adapter, got nil")
	}
	var nilAdapter *ModelRegistryAdapter
	if _, err := nilAdapter.Resolve(IngressOpenAI, "anything", ""); err == nil {
		t.Fatal("expected error from nil adapter, got nil")
	}
}

func TestModelRegistryAdapterProjectsExactCatalogFields(t *testing.T) {
	registry, err := adaptermodel.NewRegistry(resolverCatalogConfig())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	adapter := NewModelRegistryAdapter(registry)
	got, err := adapter.Resolve(IngressOpenAI, "gpt-alias", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Provider != ProviderCodex || got.Model != "gpt-wire" {
		t.Fatalf("provider/model = %q/%q", got.Provider, got.Model)
	}
	if got.Instructions != "follow repo conventions" {
		t.Fatalf("instructions = %q", got.Instructions)
	}
	if got.Effort != Effort("future-tier") {
		t.Fatalf("effort = %q, want future-tier", got.Effort)
	}
	if got.ToolsCapability == nil || !*got.ToolsCapability {
		t.Fatalf("tools capability = %v, want known true", got.ToolsCapability)
	}
	if got.VisionCapability == nil || *got.VisionCapability {
		t.Fatalf("vision capability = %v, want known false", got.VisionCapability)
	}
	if got.Context != 200000 || got.MaxOutputTokens != 16000 {
		t.Fatalf("context/output = %d/%d", got.Context, got.MaxOutputTokens)
	}
	if got.Family != "standard" || got.Pricing.InputPerMTok != 2.5 || got.Pricing.OutputPerMTok != 15 {
		t.Fatalf("profile/pricing = %q/%+v", got.Family, got.Pricing)
	}
}

func TestModelRegistryAdapterPreservesWildcardUnknownCapabilities(t *testing.T) {
	cfg := resolverCatalogConfig()
	cfg.ModelRoutes = []config.AdapterModelRoute{{
		Match:            "gpt-*",
		Surfaces:         []config.AdapterIngressSurface{config.AdapterIngressCursor},
		Provider:         config.AdapterModelProviderCodex,
		WireModelPolicy:  config.AdapterWireModelPolicyPreserve,
		CapabilityPolicy: config.AdapterWildcardCapabilityPolicyPassthrough,
	}}
	registry, err := adaptermodel.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := NewModelRegistryAdapter(registry).Resolve(IngressCursor, "gpt-future", "ultra")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Model != "gpt-future" || got.Effort != Effort("ultra") {
		t.Fatalf("model/effort = %q/%q", got.Model, got.Effort)
	}
	if got.ToolsCapability != nil || got.VisionCapability != nil {
		t.Fatalf("wildcard capabilities = %v/%v, want unknown", got.ToolsCapability, got.VisionCapability)
	}
}

func TestModelRegistryAdapterProjectsPassthroughOverride(t *testing.T) {
	cfg := resolverCatalogConfig()
	cfg.PassthroughOverrides = map[string]config.AdapterPassthroughOverride{
		"vendor-x": {
			BaseURL:   "https://upstream.invalid/v1",
			APIKeyEnv: "UPSTREAM_API_KEY",
			Model:     "upstream-model",
		},
	}
	cfg.Models["vendor-alias"] = config.AdapterModelDeclaration{
		Provider:            config.AdapterModelProviderPassthroughOverride,
		WireModel:           "upstream-model",
		Profile:             "standard",
		PassthroughOverride: "vendor-x",
	}
	registry, err := adaptermodel.NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	got, err := NewModelRegistryAdapter(registry).Resolve(IngressOpenAI, "vendor-alias", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Provider != ProviderPassthrough || got.PassthroughOverrideName != "vendor-x" {
		t.Fatalf("provider/override = %q/%q", got.Provider, got.PassthroughOverrideName)
	}
	if got.PassthroughOverride.BaseURL != "https://upstream.invalid/v1" ||
		got.PassthroughOverride.APIKeyEnv != "UPSTREAM_API_KEY" ||
		got.PassthroughOverride.Model != "upstream-model" {
		t.Fatalf("passthrough override snapshot = %+v", got.PassthroughOverride)
	}
}

func resolverCatalogConfig() config.AdapterConfig {
	tools := true
	vision := false
	return config.AdapterConfig{
		DefaultModel: "gpt-exact",
		ClientIdentity: config.AdapterClientIdentity{
			SystemPromptPrefix:      "prefix",
			StainlessPackageVersion: "test",
			StainlessRuntime:        "go",
			StainlessRuntimeVersion: "1.0",
			CCVersion:               "1.0.0",
			CCEntrypoint:            "test",
		},
		Codex: config.AdapterCodex{Enabled: true},
		ModelProfiles: map[string]config.AdapterModelProfile{
			"standard": {
				Contexts:         []config.AdapterModelProfileContext{{Name: "standard", Tokens: 200000}},
				MaxOutputTokens:  16000,
				ReasoningEfforts: []config.AdapterReasoningEffort{"future-tier"},
				DefaultEffort:    "future-tier",
				SupportsTools:    &tools,
				SupportsVision:   &vision,
			},
		},
		Models: map[string]config.AdapterModelDeclaration{
			"gpt-exact": {
				Provider:     config.AdapterModelProviderCodex,
				WireModel:    "gpt-wire",
				Profile:      "standard",
				Instructions: "follow repo conventions",
				Pricing:      config.AdapterModelPricing{InputPerMTok: 2.5, OutputPerMTok: 15},
				Aliases:      []config.AdapterModelAlias{{ID: "gpt-alias"}},
			},
		},
	}
}
