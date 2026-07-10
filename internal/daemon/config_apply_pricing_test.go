package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"goodkind.io/clyde/internal/adapter"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
)

func TestApplyConfigInProcessReplacesModelLocalPricing(t *testing.T) {
	recorder := newProviderStatsRecorder(adapterruntime.NewPricingTable(map[string]config.AdapterModelPricing{
		"claude-hot":         {InputPerMTok: 1},
		"legacy-global-only": {InputPerMTok: 9},
	}))
	runtime := &runtimeServices{
		stats: recorder,
	}
	oldConfig := config.NewConfigWithDefaults()
	runtime.currentConfig.Store(oldConfig)

	recorder.record(context.Background(), completedEvent("anthropic", "claude-hot", 1000, 0, 0, 0))
	newConfig := config.NewConfigWithDefaults()
	newConfig.Adapter = hotApplyAdapterConfig()
	model := newConfig.Adapter.Models["claude-hot"]
	model.Pricing = config.AdapterModelPricing{InputPerMTok: 4}
	newConfig.Adapter.Models["claude-hot"] = model
	if err := runtime.applyConfigInProcess(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), newConfig); err != nil {
		t.Fatalf("applyConfigInProcess: %v", err)
	}

	recorder.record(context.Background(), completedEvent("anthropic", "claude-hot", 1000, 0, 0, 0))
	recorder.record(context.Background(), completedEvent("anthropic", "legacy-global-only", 1000, 0, 0, 0))
	snapshot, _ := recorder.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("stats entries = %d, want 1", len(snapshot))
	}
	// The first request uses $1/MTok and the second uses the hot-applied
	// $4/MTok model-local rate. The removed legacy-only entry adds no cost.
	if snapshot[0].EstimatedCostMicrocents != 500000 {
		t.Fatalf("estimated cost = %d, want 500000", snapshot[0].EstimatedCostMicrocents)
	}
	if got := runtime.currentConfig.Load(); got != newConfig {
		t.Fatalf("current config = %p, want %p", got, newConfig)
	}
}

func TestApplyConfigInProcessKeepsPricingWhenAdapterApplyFails(t *testing.T) {
	adapterConfig := hotApplyAdapterConfig()
	server, err := adapter.New(context.Background(), adapterConfig, config.LoggingConfig{}, adapter.Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("adapter.New: %v", err)
	}
	recorder := newProviderStatsRecorder(adapterruntime.NewPricingTable(map[string]config.AdapterModelPricing{
		"claude-hot": {InputPerMTok: 1},
	}))
	runtime := &runtimeServices{
		adapter: server,
		stats:   recorder,
	}
	oldConfig := config.NewConfigWithDefaults()
	oldConfig.Adapter = adapterConfig
	runtime.currentConfig.Store(oldConfig)

	newConfig := config.NewConfigWithDefaults()
	newConfig.Adapter = adapterConfig
	newConfig.Adapter.DefaultModel = "missing-model"
	model := newConfig.Adapter.Models["claude-hot"]
	model.Pricing = config.AdapterModelPricing{InputPerMTok: 4}
	newConfig.Adapter.Models["claude-hot"] = model
	if err := runtime.applyConfigInProcess(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), newConfig); err == nil {
		t.Fatal("applyConfigInProcess returned nil error")
	}

	recorder.record(context.Background(), completedEvent("anthropic", "claude-hot", 1000, 0, 0, 0))
	snapshot, _ := recorder.snapshot()
	if len(snapshot) != 1 || snapshot[0].EstimatedCostMicrocents != 100000 {
		t.Fatalf("stats after failed apply = %+v, want old $1/MTok rate", snapshot)
	}
	if got := runtime.currentConfig.Load(); got != oldConfig {
		t.Fatalf("current config changed after failed apply: got %p want %p", got, oldConfig)
	}
}

func hotApplyAdapterConfig() config.AdapterConfig {
	tools := true
	vision := true
	return config.AdapterConfig{
		DefaultModel: "claude-hot",
		ClientIdentity: config.AdapterClientIdentity{
			SystemPromptPrefix:      "prefix",
			StainlessPackageVersion: "test",
			StainlessRuntime:        "go",
			StainlessRuntimeVersion: "1.0",
			CCVersion:               "1.0.0",
			CCEntrypoint:            "test",
		},
		Anthropic: config.AdapterAnthropic{
			Enabled: true,
			OAuth: config.AdapterOAuth{
				MessagesURL:      "https://example.invalid/v1/messages",
				AnthropicBeta:    "test-beta",
				AnthropicVersion: "2023-06-01",
			},
		},
		ModelProfiles: map[string]config.AdapterModelProfile{
			"standard": {
				Contexts:         []config.AdapterModelProfileContext{{Name: "standard", Tokens: 200000}},
				MaxOutputTokens:  16000,
				ReasoningEfforts: []config.AdapterReasoningEffort{"medium"},
				DefaultEffort:    "medium",
				SupportsTools:    &tools,
				SupportsVision:   &vision,
			},
		},
		Models: map[string]config.AdapterModelDeclaration{
			"claude-hot": {
				Provider:  config.AdapterModelProviderAnthropic,
				WireModel: "claude-hot",
				Profile:   "standard",
				Pricing:   config.AdapterModelPricing{InputPerMTok: 1},
			},
		},
	}
}
