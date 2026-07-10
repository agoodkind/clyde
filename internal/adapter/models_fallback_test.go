package adapter

import "goodkind.io/clyde/internal/config"

func validClientIdentity() config.AdapterClientIdentity {
	return config.AdapterClientIdentity{
		BetaHeader:              "x",
		UserAgent:               "y",
		SystemPromptPrefix:      "z",
		StainlessPackageVersion: "0",
		StainlessRuntime:        "node",
		StainlessRuntimeVersion: "v0",
		CCVersion:               "1.0.0",
		CCEntrypoint:            "ci",
	}
}

func baseConfig() config.AdapterConfig {
	tools := true
	vision := true
	return config.AdapterConfig{
		DefaultModel:   "clyde-haiku-4.5-medium",
		ClientIdentity: validClientIdentity(),
		Anthropic: config.AdapterAnthropic{
			Enabled: true,
			OAuth: config.AdapterOAuth{
				MessagesURL:      "https://example.invalid/v1/messages",
				AnthropicBeta:    "test-beta",
				AnthropicVersion: "2023-06-01",
			},
		},
		ModelProfiles: map[string]config.AdapterModelProfile{
			"haiku": {
				Contexts: []config.AdapterModelProfileContext{{
					Name:   "standard",
					Tokens: 200000,
				}},
				MaxOutputTokens:  16000,
				ReasoningEfforts: []config.AdapterReasoningEffort{EffortMedium},
				DefaultEffort:    EffortMedium,
				SupportsTools:    &tools,
				SupportsVision:   &vision,
			},
		},
		Models: map[string]config.AdapterModelDeclaration{
			"clyde-haiku-4.5-medium": {
				Provider:  config.AdapterModelProviderAnthropic,
				WireModel: "claude-haiku-4-5-20251001",
				Profile:   "haiku",
				Aliases: []config.AdapterModelAlias{{
					ID: "clyde-haiku-4-5",
				}},
			},
		},
		ModelRoutes: []config.AdapterModelRoute{{
			Match:            "claude-*",
			Surfaces:         []config.AdapterIngressSurface{config.AdapterIngressCursor, config.AdapterIngressOpenAI, config.AdapterIngressAnthropic},
			Provider:         config.AdapterModelProviderAnthropic,
			WireModelPolicy:  config.AdapterWireModelPolicyPreserve,
			CapabilityPolicy: config.AdapterWildcardCapabilityPolicyPassthrough,
		}},
	}
}

func modelMatrixConfig() config.AdapterConfig {
	cfg := baseConfig()
	tools := true
	vision := true
	cfg.DefaultModel = "clyde-opus-4.7-medium"
	cfg.Codex.Enabled = true
	cfg.ModelProfiles["opus"] = config.AdapterModelProfile{
		Contexts: []config.AdapterModelProfileContext{
			{Name: "standard", Tokens: 200000},
			{Name: "large", Tokens: 1000000, AliasSuffix: "1m", WireSuffix: "[1m]"},
		},
		MaxOutputTokens: 128000,
		ReasoningEfforts: []config.AdapterReasoningEffort{
			EffortLow,
			EffortMedium,
			EffortHigh,
			EffortMax,
		},
		DefaultEffort:  EffortMedium,
		SupportsTools:  &tools,
		SupportsVision: &vision,
		ThinkingProfiles: map[string]config.AdapterModelThinkingProfile{
			"adaptive": {Mode: config.AdapterThinkingModeAdaptive},
			"disabled": {Mode: config.AdapterThinkingModeDisabled},
			"enabled":  {Mode: config.AdapterThinkingModeEnabled, BudgetTokens: 127999},
		},
	}
	cfg.ModelProfiles["codex"] = config.AdapterModelProfile{
		Contexts: []config.AdapterModelProfileContext{{
			Name:   "large",
			Tokens: 1000000,
		}},
		MaxOutputTokens: 128000,
		ReasoningEfforts: []config.AdapterReasoningEffort{
			EffortLow,
			EffortMedium,
			EffortHigh,
			EffortXHigh,
		},
		DefaultEffort:  EffortMedium,
		SupportsTools:  &tools,
		SupportsVision: &vision,
	}
	cfg.Models = map[string]config.AdapterModelDeclaration{
		"clyde-opus-4.7-medium": {
			Provider:  config.AdapterModelProviderAnthropic,
			WireModel: "claude-opus-4-7",
			Profile:   "opus",
			Aliases: []config.AdapterModelAlias{
				{ID: "clyde-opus-4.7-medium-thinking", ReasoningEffort: EffortMedium, ThinkingProfile: "adaptive"},
				{ID: "clyde-opus-4.7-high-thinking", ReasoningEffort: EffortHigh, ThinkingProfile: "adaptive"},
				{ID: "clyde-opus-4.7-1m-medium-thinking", Advertise: true, ReasoningEffort: EffortMedium, Context: "large", ThinkingProfile: "adaptive"},
				{ID: "clyde-opus-4.7-1m-high-thinking", ReasoningEffort: EffortHigh, Context: "large", ThinkingProfile: "adaptive"},
			},
		},
		"clyde-opus-4.6-medium": {
			Provider:  config.AdapterModelProviderAnthropic,
			WireModel: "claude-opus-4-6",
			Profile:   "opus",
			Aliases: []config.AdapterModelAlias{
				{ID: "clyde-opus-4.6-medium-thinking", ReasoningEffort: EffortMedium, ThinkingProfile: "enabled"},
			},
		},
		"gpt-5.4": {
			Provider:  config.AdapterModelProviderCodex,
			WireModel: "gpt-5.4",
			Profile:   "codex",
			Aliases: []config.AdapterModelAlias{
				{ID: "clyde-gpt-5.4-1m-medium", ReasoningEffort: EffortMedium},
				{ID: "clyde-gpt-5.4-1m-high", ReasoningEffort: EffortHigh},
			},
		},
	}
	return cfg
}

func addTestCodexModel(cfg *config.AdapterConfig) {
	if cfg == nil {
		return
	}
	tools := true
	vision := true
	cfg.Codex.Enabled = true
	if cfg.ModelProfiles == nil {
		cfg.ModelProfiles = make(map[string]config.AdapterModelProfile)
	}
	cfg.ModelProfiles["test-codex"] = config.AdapterModelProfile{
		Contexts:        []config.AdapterModelProfileContext{{Name: "standard", Tokens: 272000}},
		MaxOutputTokens: 128000,
		ReasoningEfforts: []config.AdapterReasoningEffort{
			EffortLow,
			EffortMedium,
			EffortHigh,
			EffortXHigh,
		},
		DefaultEffort:  EffortMedium,
		SupportsTools:  &tools,
		SupportsVision: &vision,
	}
	if cfg.Models == nil {
		cfg.Models = make(map[string]config.AdapterModelDeclaration)
	}
	cfg.Models["gpt-5.4"] = config.AdapterModelDeclaration{
		Provider:  config.AdapterModelProviderCodex,
		WireModel: "gpt-5.4",
		Profile:   "test-codex",
	}
	cfg.ModelRoutes = append(cfg.ModelRoutes, config.AdapterModelRoute{
		Match:            "gpt-*",
		Surfaces:         []config.AdapterIngressSurface{config.AdapterIngressCursor, config.AdapterIngressOpenAI},
		Provider:         config.AdapterModelProviderCodex,
		WireModelPolicy:  config.AdapterWireModelPolicyPreserve,
		CapabilityPolicy: config.AdapterWildcardCapabilityPolicyPassthrough,
	})
}
