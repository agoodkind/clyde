package model

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func TestRegistryRoutesWildcardModelBeforeOpenAICompatFallback(t *testing.T) {
	cfg := catalogTestConfig()
	cfg.Codex.Enabled = true
	cfg.ModelRoutes = []config.AdapterModelRoute{{
		Match:            "gpt-*",
		Surfaces:         []config.AdapterIngressSurface{config.AdapterIngressCursor},
		Provider:         config.AdapterModelProviderCodex,
		WireModelPolicy:  config.AdapterWireModelPolicyPreserve,
		CapabilityPolicy: config.AdapterWildcardCapabilityPolicyPassthrough,
	}}
	cfg.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{
		BaseURL: "http://localhost:5400/v1",
	}

	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	resolved, effort, err := registry.Resolve(IngressCursor, "gpt-future", "ultra")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Backend != BackendCodex {
		t.Fatalf("backend = %q, want %q", resolved.Backend, BackendCodex)
	}
	if resolved.WireModel != "gpt-future" {
		t.Fatalf("wire model = %q, want gpt-future", resolved.WireModel)
	}
	if effort != "ultra" {
		t.Fatalf("effort = %q, want ultra", effort)
	}
}

func TestRegistryAnthropicWildcardUsesConfiguredDefaultWireProfile(t *testing.T) {
	cfg := exactCatalogConfig()
	cfg.Anthropic.DefaultWireProfile = "claude-code-interactive-default"
	cfg.ModelRoutes = []config.AdapterModelRoute{
		catalogRoute("claude-*", config.AdapterModelProviderAnthropic, IngressAnthropic),
	}

	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	resolved, _, err := registry.Resolve(IngressAnthropic, "claude-future", "future-tier")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.WireProfile != cfg.Anthropic.DefaultWireProfile {
		t.Fatalf("WireProfile = %q, want %q", resolved.WireProfile, cfg.Anthropic.DefaultWireProfile)
	}
}

func TestRegistryResolutionPrecedence(t *testing.T) {
	tests := []struct {
		name          string
		requested     string
		effort        string
		wantBackend   BackendID
		wantWireModel string
		wantEffort    string
	}{
		{
			name:          "canonical exact beats route",
			requested:     "gpt-exact",
			wantBackend:   BackendCodex,
			wantWireModel: "gpt-wire",
			wantEffort:    "medium",
		},
		{
			name:          "exact alias beats route",
			requested:     "gpt-alias",
			wantBackend:   BackendCodex,
			wantWireModel: "gpt-wire",
			wantEffort:    "high",
		},
		{
			name:          "first matching route beats fallback",
			requested:     "gpt-future",
			effort:        "ultra",
			wantBackend:   BackendAnthropic,
			wantWireModel: "gpt-future",
			wantEffort:    "ultra",
		},
		{
			name:          "fallback beats default for nonempty unknown",
			requested:     "unclaimed-model",
			wantBackend:   BackendPassthroughOverride,
			wantWireModel: "unclaimed-model",
		},
		{
			name:          "default applies only when omitted",
			requested:     "",
			wantBackend:   BackendCodex,
			wantWireModel: "gpt-wire",
			wantEffort:    "medium",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := exactCatalogConfig()
			cfg.ModelRoutes = []config.AdapterModelRoute{
				catalogRoute("gpt-*", config.AdapterModelProviderAnthropic, IngressCursor),
				catalogRoute("gpt-*", config.AdapterModelProviderCodex, IngressCursor),
			}
			cfg.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{
				BaseURL: "http://localhost:5400/v1",
			}
			registry, err := NewRegistry(cfg)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			resolved, effort, err := registry.Resolve(IngressCursor, test.requested, test.effort)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if resolved.Backend != test.wantBackend {
				t.Fatalf("backend = %q, want %q", resolved.Backend, test.wantBackend)
			}
			if resolved.WireModel != test.wantWireModel {
				t.Fatalf("wire model = %q, want %q", resolved.WireModel, test.wantWireModel)
			}
			if effort != test.wantEffort {
				t.Fatalf("effort = %q, want %q", effort, test.wantEffort)
			}
		})
	}
}

func TestRegistryRoutesConfiguredSurfaces(t *testing.T) {
	tests := []struct {
		name      string
		surface   IngressSurface
		model     string
		provider  BackendID
		wantError bool
	}{
		{name: "gpt cursor", surface: IngressCursor, model: "gpt-new", provider: BackendCodex},
		{name: "gpt openai", surface: IngressOpenAI, model: "gpt-new", provider: BackendCodex},
		{name: "gpt native anthropic not claimed", surface: IngressAnthropic, model: "gpt-new", wantError: true},
		{name: "claude cursor", surface: IngressCursor, model: "claude-new", provider: BackendAnthropic},
		{name: "claude openai", surface: IngressOpenAI, model: "claude-new", provider: BackendAnthropic},
		{name: "claude native anthropic", surface: IngressAnthropic, model: "claude-new", provider: BackendAnthropic},
	}
	cfg := exactCatalogConfig()
	cfg.ModelRoutes = []config.AdapterModelRoute{
		catalogRoute("gpt-*", config.AdapterModelProviderCodex, IngressCursor, IngressOpenAI),
		catalogRoute("claude-*", config.AdapterModelProviderAnthropic, IngressCursor, IngressOpenAI, IngressAnthropic),
	}
	cfg.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{}
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, effort, resolveErr := registry.Resolve(test.surface, test.model, "future-tier")
			if test.wantError {
				if resolveErr == nil {
					t.Fatal("Resolve returned nil error")
				}
				return
			}
			if resolveErr != nil {
				t.Fatalf("Resolve: %v", resolveErr)
			}
			if resolved.Backend != test.provider {
				t.Fatalf("backend = %q, want %q", resolved.Backend, test.provider)
			}
			if resolved.WireModel != test.model || resolved.Alias != test.model {
				t.Fatalf("wildcard identity = %#v, want requested model unchanged", resolved)
			}
			if effort != "future-tier" {
				t.Fatalf("effort = %q, want future-tier", effort)
			}
			if resolved.ToolsCapability != nil || resolved.VisionCapability != nil ||
				resolved.Context != 0 || resolved.MaxOutputTokens != 0 {
				t.Fatalf("wildcard capabilities must be unknown: %#v", resolved)
			}
		})
	}
}

func TestRegistryExactEffortPolicy(t *testing.T) {
	registry, err := NewRegistry(exactCatalogConfig())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	tests := []struct {
		name      string
		model     string
		effort    string
		want      string
		wantError string
		wantKind  ResolveErrorKind
	}{
		{name: "profile default", model: "gpt-exact", want: "medium"},
		{name: "caller supported", model: "gpt-exact", effort: "low", want: "low"},
		{name: "caller unsupported", model: "gpt-exact", effort: "ultra", wantError: "not supported", wantKind: ResolveErrorInvalidRequest},
		{name: "caller whitespace unsupported", model: "gpt-exact", effort: " medium ", wantError: "not supported", wantKind: ResolveErrorInvalidRequest},
		{name: "alias bound", model: "gpt-alias", want: "high"},
		{name: "alias conflict", model: "gpt-alias", effort: "low", wantError: "conflicts", wantKind: ResolveErrorInvalidRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, effort, resolveErr := registry.Resolve(IngressOpenAI, test.model, test.effort)
			if test.wantError != "" {
				if resolveErr == nil || !strings.Contains(resolveErr.Error(), test.wantError) {
					t.Fatalf("error = %v, want substring %q", resolveErr, test.wantError)
				}
				var typedErr *ResolveError
				if !errors.As(resolveErr, &typedErr) || typedErr.Kind != test.wantKind {
					t.Fatalf("error = %T/%v, want ResolveError kind %q", resolveErr, resolveErr, test.wantKind)
				}
				return
			}
			if resolveErr != nil {
				t.Fatalf("Resolve: %v", resolveErr)
			}
			if effort != test.want {
				t.Fatalf("effort = %q, want %q", effort, test.want)
			}
		})
	}
}

func TestRegistryExactEffortUsesConfiguredWireValue(t *testing.T) {
	cfg := exactCatalogConfig()
	profile := cfg.ModelProfiles["standard"]
	profile.ReasoningEfforts = append(profile.ReasoningEfforts, config.AdapterReasoningEffort("ultra"))
	profile.ReasoningEffortWireValues = map[config.AdapterReasoningEffort]config.AdapterReasoningEffort{
		"ultra": "max",
	}
	cfg.ModelProfiles["standard"] = profile

	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	resolved, effort, err := registry.Resolve(IngressOpenAI, "gpt-exact", "ultra")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if effort != "ultra" || resolved.Effort != "ultra" || resolved.WireEffort != "max" {
		t.Fatalf("effort/resolved/wire = %q/%q/%q, want ultra/ultra/max", effort, resolved.Effort, resolved.WireEffort)
	}
}

func TestRegistryWildcardEffortKeepsIdentityWireValue(t *testing.T) {
	cfg := exactCatalogConfig()
	cfg.ModelRoutes = []config.AdapterModelRoute{
		catalogRoute("gpt-*", config.AdapterModelProviderCodex, IngressOpenAI),
	}
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	resolved, effort, err := registry.Resolve(IngressOpenAI, "gpt-future", "future-tier")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if effort != "future-tier" || resolved.WireEffort != "future-tier" {
		t.Fatalf("effort/wire = %q/%q, want future-tier identity", effort, resolved.WireEffort)
	}
}

func TestRegistryUnknownModelUsesModelNotFoundKind(t *testing.T) {
	registry, err := NewRegistry(exactCatalogConfig())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, _, err = registry.Resolve(IngressOpenAI, "definitely-unknown", "")
	var typedErr *ResolveError
	if !errors.As(err, &typedErr) || typedErr.Kind != ResolveErrorModelNotFound {
		t.Fatalf("error = %T/%v, want ResolveError kind %q", err, err, ResolveErrorModelNotFound)
	}
}

func TestRegistryDisabledRouteDoesNotFallThrough(t *testing.T) {
	cfg := exactCatalogConfig()
	cfg.Codex.Enabled = false
	cfg.Models = map[string]config.AdapterModelDeclaration{}
	cfg.DefaultModel = ""
	cfg.ModelRoutes = []config.AdapterModelRoute{
		catalogRoute("gpt-*", config.AdapterModelProviderCodex, IngressOpenAI),
	}
	cfg.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{
		BaseURL: "http://localhost:5400/v1",
	}
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, _, err = registry.Resolve(IngressOpenAI, "gpt-new", "ultra")
	if err == nil || !strings.Contains(err.Error(), "disabled provider") {
		t.Fatalf("error = %v, want disabled provider", err)
	}
}

func TestRegistryTreatsDirectOAuthAsAnthropicProviderEnabled(t *testing.T) {
	cfg := exactCatalogConfig()
	cfg.Anthropic.Enabled = false
	cfg.DirectOAuth = true
	cfg.Models["claude-exact"] = config.AdapterModelDeclaration{
		Provider:  config.AdapterModelProviderAnthropic,
		WireModel: "claude-wire",
		Profile:   "standard",
	}
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	resolved, _, err := registry.Resolve(IngressAnthropic, "claude-exact", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Backend != BackendAnthropic {
		t.Fatalf("backend = %q, want %q", resolved.Backend, BackendAnthropic)
	}
}

func TestRegistryListsOnlyAdvertisedExactEntries(t *testing.T) {
	cfg := exactCatalogConfig()
	cfg.ModelRoutes = []config.AdapterModelRoute{
		catalogRoute("gpt-*", config.AdapterModelProviderCodex, IngressOpenAI),
	}
	cfg.Models["gpt-hidden"] = config.AdapterModelDeclaration{
		Provider:  config.AdapterModelProviderCodex,
		WireModel: "gpt-hidden-wire",
		Profile:   "standard",
		Advertise: false,
	}
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	aliases := make([]string, 0, len(registry.List()))
	for _, resolved := range registry.List() {
		aliases = append(aliases, resolved.Alias)
	}
	if !slices.Equal(aliases, []string{"gpt-alias", "gpt-exact"}) {
		t.Fatalf("advertised aliases = %v, want [gpt-alias gpt-exact]", aliases)
	}
	if slices.Contains(aliases, "gpt-new") || slices.Contains(aliases, "gpt-hidden") {
		t.Fatalf("wildcard or hidden model leaked into catalog: %v", aliases)
	}
}

func TestRegistryGeneratesAliasesOnlyFromConfiguredDimensions(t *testing.T) {
	cfg := exactCatalogConfig()
	declaration := cfg.Models["gpt-exact"]
	declaration.Aliases = nil
	declaration.GeneratedAliases = config.AdapterModelGeneratedAliases{
		Prefix:     "operator-prefix",
		Advertise:  true,
		Dimensions: []config.AdapterGeneratedAliasDimension{config.AdapterGeneratedAliasDimensionReasoningEffort},
	}
	cfg.Models["gpt-exact"] = declaration
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, alias := range []string{"operator-prefix-low", "operator-prefix-medium", "operator-prefix-high"} {
		resolved, _, resolveErr := registry.Resolve(IngressOpenAI, alias, "")
		if resolveErr != nil {
			t.Fatalf("Resolve(%q): %v", alias, resolveErr)
		}
		if resolved.WireModel != "gpt-wire" {
			t.Fatalf("Resolve(%q) wire model = %q", alias, resolved.WireModel)
		}
	}
	if _, _, err := registry.Resolve(IngressOpenAI, "clyde-gpt-exact-high", ""); err == nil {
		t.Fatal("compiled GPT alias naming rule unexpectedly resolved")
	}
}

func TestRegistryRejectsGeneratedAliasCollision(t *testing.T) {
	cfg := exactCatalogConfig()
	declaration := cfg.Models["gpt-exact"]
	declaration.GeneratedAliases = config.AdapterModelGeneratedAliases{
		Prefix:     "gpt",
		Dimensions: []config.AdapterGeneratedAliasDimension{config.AdapterGeneratedAliasDimensionReasoningEffort},
	}
	cfg.Models["gpt-exact"] = declaration
	cfg.Models["gpt-low"] = config.AdapterModelDeclaration{
		Provider:  config.AdapterModelProviderCodex,
		WireModel: "collision",
		Profile:   "standard",
	}
	_, err := NewRegistry(cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate exact model alias") {
		t.Fatalf("error = %v, want generated alias collision", err)
	}
}

func catalogTestConfig() config.AdapterConfig {
	return config.AdapterConfig{
		ClientIdentity: config.AdapterClientIdentity{
			SystemPromptPrefix:      "prefix",
			StainlessPackageVersion: "test",
			StainlessRuntime:        "go",
			StainlessRuntimeVersion: "1.0",
			CCVersion:               "1.0.0",
			CCEntrypoint:            "test",
		},
	}
}

func exactCatalogConfig() config.AdapterConfig {
	cfg := catalogTestConfig()
	tools := true
	vision := false
	cfg.DefaultModel = "gpt-exact"
	cfg.Codex.Enabled = true
	cfg.Anthropic.Enabled = true
	cfg.Anthropic.OAuth = config.AdapterOAuth{
		MessagesURL:      "https://example.invalid/v1/messages",
		AnthropicBeta:    "test-beta",
		AnthropicVersion: "2023-06-01",
	}
	cfg.ModelProfiles = map[string]config.AdapterModelProfile{
		"standard": {
			Contexts: []config.AdapterModelProfileContext{{
				Name:   "standard",
				Tokens: 100,
			}},
			MaxOutputTokens:  10,
			ReasoningEfforts: []config.AdapterReasoningEffort{"low", "medium", "high"},
			DefaultEffort:    "medium",
			SupportsTools:    &tools,
			SupportsVision:   &vision,
		},
	}
	cfg.Models = map[string]config.AdapterModelDeclaration{
		"gpt-exact": {
			Provider:  config.AdapterModelProviderCodex,
			WireModel: "gpt-wire",
			Profile:   "standard",
			Advertise: true,
			Aliases: []config.AdapterModelAlias{{
				ID:              "gpt-alias",
				Advertise:       true,
				ReasoningEffort: "high",
			}},
		},
		"claude-exact": {
			Provider:  config.AdapterModelProviderAnthropic,
			WireModel: "claude-wire",
			Profile:   "standard",
		},
	}
	return cfg
}

func catalogRoute(
	pattern string,
	provider config.AdapterModelProvider,
	surfaces ...IngressSurface,
) config.AdapterModelRoute {
	return config.AdapterModelRoute{
		Match:            pattern,
		Surfaces:         surfaces,
		Provider:         provider,
		WireModelPolicy:  config.AdapterWireModelPolicyPreserve,
		CapabilityPolicy: config.AdapterWildcardCapabilityPolicyPassthrough,
	}
}
