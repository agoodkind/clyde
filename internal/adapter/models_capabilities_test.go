package adapter

import (
	"strings"
	"testing"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	"goodkind.io/clyde/internal/config"
)

var (
	testBoolTrue  = true
	testBoolFalse = false
)

func TestNewRegistryExactCapabilitiesPropagated(t *testing.T) {
	cfg := baseConfig()
	profile := cfg.ModelProfiles["haiku"]
	profile.SupportsTools = &testBoolTrue
	profile.SupportsVision = &testBoolFalse
	cfg.ModelProfiles["haiku"] = profile
	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	resolved, _, err := registry.Resolve(
		adaptermodel.IngressOpenAI,
		"clyde-haiku-4.5-medium",
		"",
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.ToolsCapability == nil || !*resolved.ToolsCapability {
		t.Fatalf("tools capability = %v, want known true", resolved.ToolsCapability)
	}
	if resolved.VisionCapability == nil || *resolved.VisionCapability {
		t.Fatalf("vision capability = %v, want known false", resolved.VisionCapability)
	}
}

func TestNewRegistryLogprobsValidation(t *testing.T) {
	tests := []struct {
		value   string
		wantErr string
	}{
		{value: "drop"},
		{value: "reject"},
		{value: "verbatim", wantErr: "invalid"},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Logprobs.Anthropic = test.value
			_, err := NewRegistry(cfg)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("NewRegistry: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

var _ config.AdapterReasoningEffort = EffortMedium
