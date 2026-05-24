package adapter

import (
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

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

// baseConfig returns an AdapterConfig that passes the live NewRegistry
// validation for family expansion and model resolution tests.
func baseConfig() config.AdapterConfig {
	return config.AdapterConfig{
		DefaultModel:   "clyde-haiku-4.5-medium",
		ClientIdentity: validClientIdentity(),
		Families: map[string]config.AdapterFamily{
			"haiku-4-5": {
				AliasPrefix:     "haiku-4.5",
				Model:           "claude-haiku-4-5-20251001",
				Efforts:         []string{EffortMedium},
				ThinkingModes:   []string{"default"},
				MaxOutputTokens: 16000,
				SupportsTools:   &testBoolTrue,
				SupportsVision:  &testBoolTrue,
				Contexts: []config.AdapterModelContext{
					{Tokens: 200000},
				},
			},
		},
	}
}

func TestNewRegistryDirectOAuthRequiresOAuthBlock(t *testing.T) {
	cfg := baseConfig()
	cfg.DirectOAuth = true
	_, err := NewRegistry(cfg)
	if err == nil {
		t.Fatal("expected error when direct_oauth without [adapter.anthropic.oauth]")
	}
	if !strings.Contains(err.Error(), "token_url") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewRegistryRejectsFallbackModelBackend(t *testing.T) {
	cfg := baseConfig()
	cfg.Models = map[string]config.AdapterModel{
		"legacy-fallback": {
			Backend: "fallback",
			Model:   "opus",
		},
	}
	if _, err := NewRegistry(cfg); err == nil {
		t.Fatal("expected fallback backend model to be rejected")
	} else if !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("err = %v", err)
	}
}

func modelMatrixConfig() config.AdapterConfig {
	cfg := baseConfig()
	cfg.DefaultModel = "clyde-opus-4.7-medium"
	cfg.Families["opus-4-7"] = config.AdapterFamily{
		AliasPrefix:     "opus-4.7",
		Model:           "claude-opus-4-7",
		Efforts:         []string{EffortLow, EffortMedium, EffortHigh, EffortMax},
		ThinkingModes:   []string{ThinkingDefault, ThinkingAdaptive, ThinkingEnabled, ThinkingDisabled},
		MaxOutputTokens: 128000,
		SupportsTools:   &testBoolTrue,
		SupportsVision:  &testBoolTrue,
		Contexts: []config.AdapterModelContext{
			{Tokens: 200000},
			{Tokens: 1000000, AliasSuffix: "1m", WireSuffix: "[1m]"},
		},
	}
	cfg.Families["opus-4-6"] = config.AdapterFamily{
		AliasPrefix:     "opus-4.6",
		Model:           "claude-opus-4-6",
		Efforts:         []string{EffortLow, EffortMedium, EffortHigh, EffortMax},
		ThinkingModes:   []string{ThinkingDefault, ThinkingAdaptive, ThinkingEnabled, ThinkingDisabled},
		MaxOutputTokens: 128000,
		SupportsTools:   &testBoolTrue,
		SupportsVision:  &testBoolTrue,
		Contexts: []config.AdapterModelContext{
			{Tokens: 200000},
			{Tokens: 1000000, AliasSuffix: "1m", WireSuffix: "[1m]"},
		},
	}
	cfg.Codex.Enabled = true
	cfg.Codex.ModelPrefixes = []string{"gpt-", "o"}
	cfg.Codex.NativeModelRouting = "codex"
	cfg.Codex.Models = testCodexModels()
	return cfg
}

func testCodexModels() []config.AdapterCodexModel {
	return []config.AdapterCodexModel{
		{
			AliasPrefix:     "gpt-5.4",
			Model:           "gpt-5.4",
			Efforts:         []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh},
			MaxOutputTokens: 128000,
			Contexts: []config.AdapterCodexModelContext{
				{
					Tokens:                  1000000,
					ObservedTokens:          272000,
					AliasSuffix:             "1m",
					AdvertisedNativeAliases: []string{"gpt-5.4"},
				},
			},
		},
		{
			AliasPrefix:     "codex-5.5",
			Model:           "gpt-5.5",
			Efforts:         []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh},
			MaxOutputTokens: 128000,
			Contexts: []config.AdapterCodexModelContext{
				{Tokens: 272000, NativeAliases: []string{"gpt-5.5"}},
			},
		},
		{
			AliasPrefix:     "codex-5.3",
			Model:           "gpt-5.3-codex",
			Efforts:         []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh},
			MaxOutputTokens: 128000,
			Contexts: []config.AdapterCodexModelContext{
				{Tokens: 272000, NativeAliases: []string{"gpt-5.3-codex"}},
			},
		},
		{
			AliasPrefix:     "codex-5.3-spark",
			Model:           "gpt-5.3-codex-spark",
			Efforts:         []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh},
			MaxOutputTokens: 128000,
			Contexts: []config.AdapterCodexModelContext{
				{Tokens: 272000, NativeAliases: []string{"gpt-5.3-codex-spark"}},
			},
		},
	}
}

func TestNewRegistryPropagatesInstructionsFromConfig(t *testing.T) {
	cfg := modelMatrixConfig()
	cfg.Models = map[string]config.AdapterModel{
		"custom-model": {
			Model:        "claude-custom",
			Instructions: "model prompt",
		},
	}
	cfg.Families["opus-4-7"] = config.AdapterFamily{
		AliasPrefix:     "opus-4.7",
		Model:           "claude-opus-4-7",
		Instructions:    "family prompt",
		Efforts:         []string{EffortMedium},
		ThinkingModes:   []string{ThinkingDefault, ThinkingEnabled, ThinkingDisabled},
		MaxOutputTokens: 128000,
		SupportsTools:   &testBoolTrue,
		SupportsVision:  &testBoolTrue,
		Contexts: []config.AdapterModelContext{
			{Tokens: 200000},
		},
	}
	cfg.Codex.Models = []config.AdapterCodexModel{{
		AliasPrefix:     "gpt-5.4",
		Model:           "gpt-5.4",
		Instructions:    "codex prompt",
		Efforts:         []string{EffortMedium},
		MaxOutputTokens: 128000,
		Contexts: []config.AdapterCodexModelContext{{
			Tokens:                  1000000,
			ObservedTokens:          272000,
			AliasSuffix:             "1m",
			AdvertisedNativeAliases: []string{"gpt-5.4"},
		}},
	}}

	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	familyModel, _, err := r.Resolve("clyde-opus-4.7-medium", "")
	if err != nil {
		t.Fatalf("Resolve family: %v", err)
	}
	if familyModel.Instructions != "family prompt" {
		t.Fatalf("family instructions = %q want family prompt", familyModel.Instructions)
	}

	configuredModel, _, err := r.Resolve("custom-model", "")
	if err != nil {
		t.Fatalf("Resolve configured model: %v", err)
	}
	if configuredModel.Instructions != "model prompt" {
		t.Fatalf("configured model instructions = %q want model prompt", configuredModel.Instructions)
	}

	codexModel, _, err := r.Resolve("clyde-gpt-5.4-1m-medium", "")
	if err != nil {
		t.Fatalf("Resolve codex model: %v", err)
	}
	if codexModel.Instructions != "codex prompt" {
		t.Fatalf("codex instructions = %q want codex prompt", codexModel.Instructions)
	}
}

func TestResolveRoutesCodexModelPrefixes(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.NativeModelRouting = "codex"
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	m, effort, err := r.Resolve("gpt-5.4", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Backend != BackendCodex {
		t.Fatalf("backend = %q want %q", m.Backend, BackendCodex)
	}
	if m.Alias != "gpt-5.4" {
		t.Fatalf("alias = %q want gpt-5.4", m.Alias)
	}
	if effort != "" {
		t.Fatalf("effort = %q want empty", effort)
	}
}

func TestResolveRoutesNativeCodexByDefaultWhenCodexEnabled(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.ModelPrefixes = []string{"gpt-", "o"}
	cfg.Codex.Models = testCodexModels()
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	m, effort, err := r.Resolve("gpt-5.4", "medium")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Backend != BackendCodex {
		t.Fatalf("backend = %q want %q", m.Backend, BackendCodex)
	}
	if m.ClaudeModel != "gpt-5.4" {
		t.Fatalf("ClaudeModel = %q want gpt-5.4", m.ClaudeModel)
	}
	if m.Context != 1000000 {
		t.Fatalf("Context = %d want 1000000", m.Context)
	}
	if effort != "medium" {
		t.Fatalf("effort = %q want medium", effort)
	}
}

func TestResolveRejectsNativeCodexWhenRoutingExplicitlyOff(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.NativeModelRouting = "off"
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, _, err := r.Resolve("gpt-5.4", ""); err == nil {
		t.Fatalf("expected native gpt alias to be rejected when routing is off")
	} else if !strings.Contains(err.Error(), "native model routing is off") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListIncludesNativeCodexModelsWhenRoutable(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.Models = testCodexModels()
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	var gpt54 *ResolvedModel
	for _, m := range r.List() {
		if m.Alias == "gpt-5.4" {
			copy := m
			gpt54 = &copy
			break
		}
	}
	if gpt54 == nil {
		t.Fatalf("gpt-5.4 missing from model list")
	}
	if gpt54.Backend != BackendCodex {
		t.Fatalf("backend = %q want %q", gpt54.Backend, BackendCodex)
	}
	if gpt54.Context != 1000000 {
		t.Fatalf("Context = %d want 1000000", gpt54.Context)
	}
}

func TestListIncludesClydeGPTCodexEffortAliasesWhenRoutable(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.Models = testCodexModels()
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	models := make(map[string]ResolvedModel)
	for _, model := range r.List() {
		models[model.Alias] = model
	}
	cases := []struct {
		alias       string
		wantModel   string
		wantContext int
	}{
		{alias: "clyde-gpt-5.4-1m-low", wantModel: "gpt-5.4", wantContext: 1000000},
		{alias: "clyde-gpt-5.4-1m-medium", wantModel: "gpt-5.4", wantContext: 1000000},
		{alias: "clyde-gpt-5.4-1m-high", wantModel: "gpt-5.4", wantContext: 1000000},
		{alias: "clyde-gpt-5.4-1m-xhigh", wantModel: "gpt-5.4", wantContext: 1000000},
		{alias: "clyde-codex-5.5-low", wantModel: "gpt-5.5", wantContext: 272000},
		{alias: "clyde-codex-5.5-medium", wantModel: "gpt-5.5", wantContext: 272000},
		{alias: "clyde-codex-5.5-high", wantModel: "gpt-5.5", wantContext: 272000},
		{alias: "clyde-codex-5.5-xhigh", wantModel: "gpt-5.5", wantContext: 272000},
	}
	for _, tc := range cases {
		model, ok := models[tc.alias]
		if !ok {
			t.Fatalf("%s missing from model list", tc.alias)
		}
		if model.Backend != BackendCodex {
			t.Fatalf("%s backend = %q want %q", tc.alias, model.Backend, BackendCodex)
		}
		if model.ClaudeModel != tc.wantModel {
			t.Fatalf("%s ClaudeModel = %q want %q", tc.alias, model.ClaudeModel, tc.wantModel)
		}
		if model.Context != tc.wantContext {
			t.Fatalf("%s Context = %d want %d", tc.alias, model.Context, tc.wantContext)
		}
	}
	if _, ok := models["clyde-codex-5.5-1m-high"]; ok {
		t.Fatalf("clyde-codex-5.5-1m-high should not be advertised")
	}
	if _, ok := models["clyde-gpt-5.5-high"]; ok {
		t.Fatalf("clyde-gpt-5.5-high should not be advertised; Cursor mangles gpt-5.5-looking aliases")
	}
	if _, ok := models["gpt-5.5"]; ok {
		t.Fatalf("native gpt-5.5 should not be advertised; Cursor mangles native-looking 5.5 aliases")
	}
	if _, ok := models["clyde-codex-5.5"]; !ok {
		t.Fatalf("clyde-codex-5.5 bare compatibility alias missing")
	}
	if _, _, err := r.Resolve("clyde-codex-5.5", ""); err == nil {
		t.Fatalf("clyde-codex-5.5 should not resolve without explicit effort")
	}
	m, effort, err := r.Resolve("clyde-codex-5.5", EffortHigh)
	if err != nil {
		t.Fatalf("clyde-codex-5.5 should resolve when Cursor supplies effort: %v", err)
	}
	if m.Effort != "" {
		t.Fatalf("bare compatibility alias Effort = %q, want empty", m.Effort)
	}
	if effort != EffortHigh {
		t.Fatalf("effort = %q want %q", effort, EffortHigh)
	}
}

func TestResolveRoutesClydeCodexAliases(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.NativeModelRouting = "codex"
	cfg.Codex.Models = testCodexModels()
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	m, _, err := r.Resolve("clyde-codex-gpt-5.4", "")
	if err != nil {
		t.Fatalf("Resolve clyde-codex-gpt: %v", err)
	}
	if m.Backend != BackendCodex {
		t.Fatalf("backend = %q want %q", m.Backend, BackendCodex)
	}
	if m.ClaudeModel != "gpt-5.4" {
		t.Fatalf("ClaudeModel = %q want gpt-5.4", m.ClaudeModel)
	}

	m, effort, err := r.Resolve("clyde-codex-5.5-high", "")
	if err != nil {
		t.Fatalf("Resolve clyde-codex-5.5-high: %v", err)
	}
	if m.Backend != BackendCodex {
		t.Fatalf("backend = %q want %q", m.Backend, BackendCodex)
	}
	if m.ClaudeModel != "gpt-5.5" {
		t.Fatalf("ClaudeModel = %q want gpt-5.5", m.ClaudeModel)
	}
	if m.Context != 272000 {
		t.Fatalf("Context = %d want 272000", m.Context)
	}
	if effort != EffortHigh {
		t.Fatalf("effort = %q want %q", effort, EffortHigh)
	}

	m, effort, err = r.Resolve("clyde-gpt-5.4-1m-xhigh", "")
	if err != nil {
		t.Fatalf("Resolve clyde-gpt-5.4-1m-xhigh: %v", err)
	}
	if m.ClaudeModel != "gpt-5.4" {
		t.Fatalf("ClaudeModel = %q want gpt-5.4", m.ClaudeModel)
	}
	if m.Context != 1000000 {
		t.Fatalf("Context = %d want 1000000", m.Context)
	}
	if effort != EffortXHigh {
		t.Fatalf("effort = %q want %q", effort, EffortXHigh)
	}

	m, _, err = r.Resolve("gpt-5.3-codex-spark", "")
	if err != nil {
		t.Fatalf("Resolve gpt-5.3-codex-spark: %v", err)
	}
	if m.Backend != BackendCodex {
		t.Fatalf("backend = %q want %q", m.Backend, BackendCodex)
	}
	if m.ClaudeModel != "gpt-5.3-codex-spark" {
		t.Fatalf("ClaudeModel = %q want gpt-5.3-codex-spark", m.ClaudeModel)
	}

	m, effort, err = r.Resolve("gpt-5.4", "xhigh")
	if err != nil {
		t.Fatalf("Resolve gpt-5.4 xhigh: %v", err)
	}
	if m.Backend != BackendCodex {
		t.Fatalf("backend = %q want %q", m.Backend, BackendCodex)
	}
	if m.ClaudeModel != "gpt-5.4" {
		t.Fatalf("ClaudeModel = %q want gpt-5.4", m.ClaudeModel)
	}
	if m.Context != 1000000 {
		t.Fatalf("Context = %d want 1000000", m.Context)
	}
	if effort != "xhigh" {
		t.Fatalf("effort = %q want xhigh", effort)
	}

	m, effort, err = r.Resolve("gpt-5.5", "high")
	if err != nil {
		t.Fatalf("Resolve gpt-5.5 high: %v", err)
	}
	if m.Backend != BackendCodex {
		t.Fatalf("backend = %q want %q", m.Backend, BackendCodex)
	}
	if m.ClaudeModel != "gpt-5.5" {
		t.Fatalf("ClaudeModel = %q want gpt-5.5", m.ClaudeModel)
	}
	if m.Context != 272000 {
		t.Fatalf("Context = %d want 272000", m.Context)
	}
	if effort != "high" {
		t.Fatalf("effort = %q want high", effort)
	}

	m, effort, err = r.Resolve("gpt-5.3-codex", "medium")
	if err != nil {
		t.Fatalf("Resolve gpt-5.3-codex medium: %v", err)
	}
	if m.Backend != BackendCodex {
		t.Fatalf("backend = %q want %q", m.Backend, BackendCodex)
	}
	if m.ClaudeModel != "gpt-5.3-codex" {
		t.Fatalf("ClaudeModel = %q want gpt-5.3-codex", m.ClaudeModel)
	}
	if m.Context != 272000 {
		t.Fatalf("Context = %d want 272000", m.Context)
	}
	if effort != "medium" {
		t.Fatalf("effort = %q want medium", effort)
	}
}

func TestResolveExplicitConfiguredClydeCodexAliasWinsOverDynamicPrefix(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.NativeModelRouting = "codex"
	cfg.Models = map[string]config.AdapterModel{
		"clyde-codex-5.5": {
			Backend: "codex",
			Model:   "gpt-5.5",
			Context: 272000,
			Efforts: []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh},
		},
	}
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	m, effort, err := r.Resolve("clyde-codex-5.5", EffortHigh)
	if err != nil {
		t.Fatalf("Resolve clyde-codex-5.5: %v", err)
	}
	if m.Backend != BackendCodex {
		t.Fatalf("backend = %q want %q", m.Backend, BackendCodex)
	}
	if m.ClaudeModel != "gpt-5.5" {
		t.Fatalf("ClaudeModel = %q want gpt-5.5", m.ClaudeModel)
	}
	if m.Context != 272000 {
		t.Fatalf("Context = %d want 272000", m.Context)
	}
	if effort != EffortHigh {
		t.Fatalf("effort = %q want %q", effort, EffortHigh)
	}
}

func TestResolveDoesNotRouteCodexWhenDisabled(t *testing.T) {
	cfg := baseConfig()
	cfg.Codex.Enabled = false
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	m, _, err := r.Resolve("gpt-5.4", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Backend == BackendCodex {
		t.Fatalf("backend = %q want non-codex fallback/default", m.Backend)
	}
}

func TestResolveUnknownModelUsesOpenAICompatPassthrough(t *testing.T) {
	cfg := baseConfig()
	cfg.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{
		BaseURL:   "http://[::1]:1234/v1",
		APIKeyEnv: "OPENAI_API_KEY",
	}
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	m, effort, err := r.Resolve("gpt-custom", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Backend != BackendPassthroughOverride {
		t.Fatalf("backend = %q want %q", m.Backend, BackendPassthroughOverride)
	}
	if m.PassthroughOverride != "" {
		t.Fatalf("PassthroughOverride = %q want empty for direct passthrough", m.PassthroughOverride)
	}
	if m.OpenAICompatPassthrough.BaseURL != "http://[::1]:1234/v1" {
		t.Fatalf("passthrough base_url = %q", m.OpenAICompatPassthrough.BaseURL)
	}
	if effort != "" {
		t.Fatalf("effort = %q want empty", effort)
	}
}

func TestResolveConfiguredModelUsesPassthroughOverride(t *testing.T) {
	cfg := baseConfig()
	cfg.PassthroughOverrides = map[string]config.AdapterPassthroughOverride{
		"local": {
			BaseURL: "http://localhost:1234/v1",
			Model:   "local-model",
		},
	}
	cfg.Models = map[string]config.AdapterModel{
		"gpt-local": {
			PassthroughOverride: "local",
		},
	}
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	m, effort, err := r.Resolve("gpt-local", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Backend != BackendPassthroughOverride {
		t.Fatalf("backend = %q want %q", m.Backend, BackendPassthroughOverride)
	}
	if m.PassthroughOverride != "local" {
		t.Fatalf("PassthroughOverride = %q want local", m.PassthroughOverride)
	}
	if effort != "" {
		t.Fatalf("effort = %q want empty", effort)
	}
}

func TestResolveDoesNotRouteClydeOpusAliasesToCodex(t *testing.T) {
	r, err := NewRegistry(config.AdapterConfig{
		DefaultModel: "clyde-opus-4.7-medium",
		ClientIdentity: config.AdapterClientIdentity{
			BetaHeader:              "beta",
			UserAgent:               "ua",
			SystemPromptPrefix:      "prefix",
			StainlessPackageVersion: "1",
			StainlessRuntime:        "go",
			StainlessRuntimeVersion: "1",
			CCVersion:               "1",
			CCEntrypoint:            "entry",
		},
		Families: map[string]config.AdapterFamily{
			"opus-4-7": {
				AliasPrefix:     "opus-4.7",
				Model:           "claude-opus-4-7",
				Contexts:        []config.AdapterModelContext{{AliasSuffix: "", Tokens: 200000}},
				Efforts:         []string{"medium"},
				ThinkingModes:   []string{"", "enabled"},
				SupportsTools:   &testBoolTrue,
				SupportsVision:  &testBoolTrue,
				MaxOutputTokens: 8192,
			},
		},
		Codex: config.AdapterCodex{
			Enabled:       true,
			ModelPrefixes: []string{"gpt-", "o"},
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	m, _, err := r.Resolve("clyde-opus-4.7-medium-thinking", "")
	if err != nil {
		t.Fatalf("Resolve clyde-opus-4.7-medium-thinking: %v", err)
	}
	if m.Backend == BackendCodex {
		t.Fatalf("backend = %q want non-codex", m.Backend)
	}
	if m.ClaudeModel != "claude-opus-4-7" {
		t.Fatalf("ClaudeModel = %q want claude-opus-4-7", m.ClaudeModel)
	}
}

func TestResolveModelRoutingMatrix(t *testing.T) {
	r, err := NewRegistry(modelMatrixConfig())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	cases := []struct {
		alias              string
		reqEffort          string
		wantBackend        string
		wantModel          string
		wantContext        int
		wantEffort         string
		wantThinking       string
		wantSupportsTools  bool
		wantSupportsVision bool
	}{
		{
			alias:              "clyde-opus-4.7-medium",
			wantBackend:        BackendClaude,
			wantModel:          "claude-opus-4-7",
			wantContext:        200000,
			wantEffort:         EffortMedium,
			wantThinking:       ThinkingDisabled,
			wantSupportsTools:  true,
			wantSupportsVision: true,
		},
		{
			// claude-opus-4-7 with no explicit thinking_wire_mode hits
			// the registry's implicit fallback in
			// withResolvedThinkingWireMode and resolves to adaptive.
			alias:              "clyde-opus-4.7-medium-thinking",
			wantBackend:        BackendClaude,
			wantModel:          "claude-opus-4-7",
			wantContext:        200000,
			wantEffort:         EffortMedium,
			wantThinking:       ThinkingAdaptive,
			wantSupportsTools:  true,
			wantSupportsVision: true,
		},
		{
			alias:              "clyde-opus-4.7-1m-high-thinking",
			wantBackend:        BackendClaude,
			wantModel:          "claude-opus-4-7[1m]",
			wantContext:        1000000,
			wantEffort:         EffortHigh,
			wantThinking:       ThinkingAdaptive,
			wantSupportsTools:  true,
			wantSupportsVision: true,
		},
		{
			alias:              "clyde-opus-4.6-1m-max-thinking",
			wantBackend:        BackendClaude,
			wantModel:          "claude-opus-4-6[1m]",
			wantContext:        1000000,
			wantEffort:         EffortMax,
			wantThinking:       ThinkingEnabled,
			wantSupportsTools:  true,
			wantSupportsVision: true,
		},
		{
			alias:       "gpt-5.4",
			wantBackend: BackendCodex,
			wantModel:   "gpt-5.4",
			wantContext: 1000000,
		},
		{
			alias:       "gpt-5.5",
			reqEffort:   EffortMedium,
			wantBackend: BackendCodex,
			wantModel:   "gpt-5.5",
			wantContext: 272000,
			wantEffort:  EffortMedium,
		},
		{
			alias:       "gpt-5.5",
			reqEffort:   EffortXHigh,
			wantBackend: BackendCodex,
			wantModel:   "gpt-5.5",
			wantContext: 272000,
			wantEffort:  EffortXHigh,
		},
		{
			alias:       "gpt-5.4-mini",
			reqEffort:   EffortLow,
			wantBackend: BackendCodex,
			wantModel:   "gpt-5.4-mini",
			wantEffort:  EffortLow,
		},
		{
			alias:       "gpt-5.3-codex",
			reqEffort:   EffortHigh,
			wantBackend: BackendCodex,
			wantModel:   "gpt-5.3-codex",
			wantContext: 272000,
			wantEffort:  EffortHigh,
		},
		{
			alias:       "gpt-5.3-codex-spark",
			reqEffort:   EffortXHigh,
			wantBackend: BackendCodex,
			wantModel:   "gpt-5.3-codex-spark",
			wantContext: 272000,
			wantEffort:  EffortXHigh,
		},
		{
			alias:       "gpt-5.2",
			wantBackend: BackendCodex,
			wantModel:   "gpt-5.2",
		},
		{
			alias:       "o3-high",
			wantBackend: BackendCodex,
			wantModel:   "o3",
			wantEffort:  EffortHigh,
		},
	}

	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			m, effort, err := r.Resolve(tc.alias, tc.reqEffort)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.alias, err)
			}
			if m.Backend != tc.wantBackend {
				t.Fatalf("backend = %q want %q", m.Backend, tc.wantBackend)
			}
			if m.ClaudeModel != tc.wantModel {
				t.Fatalf("ClaudeModel = %q want %q", m.ClaudeModel, tc.wantModel)
			}
			if m.Context != tc.wantContext {
				t.Fatalf("Context = %d want %d", m.Context, tc.wantContext)
			}
			if effort != tc.wantEffort {
				t.Fatalf("effort = %q want %q", effort, tc.wantEffort)
			}
			if tc.wantBackend == BackendClaude {
				if m.Thinking != tc.wantThinking {
					t.Fatalf("Thinking = %q want %q", m.Thinking, tc.wantThinking)
				}
				if m.SupportsTools != tc.wantSupportsTools {
					t.Fatalf("SupportsTools = %v want %v", m.SupportsTools, tc.wantSupportsTools)
				}
				if m.SupportsVision != tc.wantSupportsVision {
					t.Fatalf("SupportsVision = %v want %v", m.SupportsVision, tc.wantSupportsVision)
				}
			}
		})
	}
}

func TestNewRegistrySupportsOpus46FamilyAliases(t *testing.T) {
	cfg := baseConfig()
	cfg.DefaultModel = "clyde-opus-4.6-medium"
	cfg.Families["opus-4-6"] = config.AdapterFamily{
		AliasPrefix:     "opus-4.6",
		Model:           "claude-opus-4-6",
		Efforts:         []string{EffortLow, EffortMedium, EffortHigh, EffortMax},
		ThinkingModes:   []string{ThinkingDefault, ThinkingAdaptive, ThinkingEnabled, ThinkingDisabled},
		MaxOutputTokens: 128000,
		SupportsTools:   &testBoolTrue,
		SupportsVision:  &testBoolTrue,
		Contexts: []config.AdapterModelContext{
			{Tokens: 200000},
			{Tokens: 1000000, AliasSuffix: "1m", WireSuffix: "[1m]"},
		},
	}

	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	cases := []struct {
		alias        string
		wantModel    string
		wantContext  int
		wantEffort   string
		wantThinking string
		wantFamily   string
	}{
		{
			alias:        "clyde-opus-4.6-medium",
			wantModel:    "claude-opus-4-6",
			wantContext:  200000,
			wantEffort:   EffortMedium,
			wantThinking: ThinkingDisabled,
			wantFamily:   "opus-4-6",
		},
		{
			alias:        "clyde-opus-4.6-1m-medium",
			wantModel:    "claude-opus-4-6[1m]",
			wantContext:  1000000,
			wantEffort:   EffortMedium,
			wantThinking: ThinkingDisabled,
			wantFamily:   "opus-4-6",
		},
		{
			alias:        "clyde-opus-4.6-1m-max-thinking",
			wantModel:    "claude-opus-4-6[1m]",
			wantContext:  1000000,
			wantEffort:   EffortMax,
			wantThinking: ThinkingEnabled,
			wantFamily:   "opus-4-6",
		},
	}

	for _, tc := range cases {
		m, ok := r.Models()[tc.alias]
		if !ok {
			t.Fatalf("alias %q missing", tc.alias)
		}
		if m.ClaudeModel != tc.wantModel {
			t.Fatalf("%s ClaudeModel = %q want %q", tc.alias, m.ClaudeModel, tc.wantModel)
		}
		if m.Context != tc.wantContext {
			t.Fatalf("%s Context = %d want %d", tc.alias, m.Context, tc.wantContext)
		}
		if m.Effort != tc.wantEffort {
			t.Fatalf("%s Effort = %q want %q", tc.alias, m.Effort, tc.wantEffort)
		}
		if m.Thinking != tc.wantThinking {
			t.Fatalf("%s Thinking = %q want %q", tc.alias, m.Thinking, tc.wantThinking)
		}
		if m.FamilySlug != tc.wantFamily {
			t.Fatalf("%s FamilySlug = %q want %q", tc.alias, m.FamilySlug, tc.wantFamily)
		}
		if !m.SupportsTools || !m.SupportsVision {
			t.Fatalf("%s capabilities = tools:%v vision:%v want true/true", tc.alias, m.SupportsTools, m.SupportsVision)
		}
	}
}

// TestThinkingWireModeExplicitOverridesImplicitFallback locks in that
// an operator can pin claude-opus-4-7 to enabled-mode wire shape by
// setting thinking_wire_mode = "enabled". The implicit
// enabled-to-adaptive fallback in withResolvedThinkingWireMode must
// not override an explicit operator choice.
func TestThinkingWireModeExplicitOverridesImplicitFallback(t *testing.T) {
	cfg := baseConfig()
	cfg.DefaultModel = "clyde-opus-4.7-medium-thinking"
	cfg.Families["opus-4-7"] = config.AdapterFamily{
		AliasPrefix:      "opus-4.7",
		Model:            "claude-opus-4-7",
		Efforts:          []string{EffortMedium},
		ThinkingModes:    []string{ThinkingDefault, ThinkingEnabled},
		ThinkingWireMode: ThinkingEnabled,
		MaxOutputTokens:  32000,
		SupportsTools:    &testBoolTrue,
		SupportsVision:   &testBoolTrue,
		Contexts:         []config.AdapterModelContext{{Tokens: 200000}},
	}

	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	m, ok := r.Models()["clyde-opus-4.7-medium-thinking"]
	if !ok {
		t.Fatalf("alias missing")
	}
	if m.Thinking != ThinkingEnabled {
		t.Fatalf("Thinking = %q want %q (explicit thinking_wire_mode must win)", m.Thinking, ThinkingEnabled)
	}
}

// TestThinkingWireModeExplicitAdaptive locks in the explicit-adaptive
// path, so an operator who sets thinking_wire_mode = "adaptive" gets
// adaptive without depending on the implicit fallback.
func TestThinkingWireModeExplicitAdaptive(t *testing.T) {
	cfg := baseConfig()
	cfg.DefaultModel = "clyde-opus-4.7-medium-thinking"
	cfg.Families["opus-4-7"] = config.AdapterFamily{
		AliasPrefix:      "opus-4.7",
		Model:            "claude-opus-4-7",
		Efforts:          []string{EffortMedium},
		ThinkingModes:    []string{ThinkingDefault, ThinkingEnabled},
		ThinkingWireMode: ThinkingAdaptive,
		MaxOutputTokens:  32000,
		SupportsTools:    &testBoolTrue,
		SupportsVision:   &testBoolTrue,
		Contexts:         []config.AdapterModelContext{{Tokens: 200000}},
	}

	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	m, ok := r.Models()["clyde-opus-4.7-medium-thinking"]
	if !ok {
		t.Fatalf("alias missing")
	}
	if m.Thinking != ThinkingAdaptive {
		t.Fatalf("Thinking = %q want %q", m.Thinking, ThinkingAdaptive)
	}
}

// TestThinkingWireModeRejectsInvalid locks in the validation path so a
// typo in thinking_wire_mode is caught at config-load time, not
// silently dropped at request time.
func TestThinkingWireModeRejectsInvalid(t *testing.T) {
	cfg := baseConfig()
	cfg.Families["opus-4-7"] = config.AdapterFamily{
		AliasPrefix:      "opus-4.7",
		Model:            "claude-opus-4-7",
		Efforts:          []string{EffortMedium},
		ThinkingModes:    []string{ThinkingDefault, ThinkingEnabled},
		ThinkingWireMode: "always-on",
		MaxOutputTokens:  32000,
		SupportsTools:    &testBoolTrue,
		SupportsVision:   &testBoolTrue,
		Contexts:         []config.AdapterModelContext{{Tokens: 200000}},
	}

	if _, err := NewRegistry(cfg); err == nil {
		t.Fatal("expected NewRegistry to reject invalid thinking_wire_mode, got nil error")
	}
}
