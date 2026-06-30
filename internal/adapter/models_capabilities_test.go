package adapter

import (
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

var (
	testBoolTrue  = true
	testBoolFalse = false
)

func TestNewRegistryCapabilitiesValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.AdapterConfig)
		wantSub string
	}{
		{
			name: "missing_supports_tools_rejected",
			mutate: func(cfg *config.AdapterConfig) {
				cfg.Families["haiku-4-5"] = config.AdapterFamily{
					AliasPrefix:     "haiku-4.5",
					Model:           "claude-haiku-4-5-20251001",
					Efforts:         []string{EffortMedium},
					ThinkingModes:   []string{"default"},
					MaxOutputTokens: 16000,
					SupportsVision:  &testBoolTrue,
					Contexts:        []config.AdapterModelContext{{Tokens: 200000}},
				}
			},
			wantSub: "supports_tools",
		},
		{
			name: "missing_supports_vision_rejected",
			mutate: func(cfg *config.AdapterConfig) {
				cfg.Families["haiku-4-5"] = config.AdapterFamily{
					AliasPrefix:     "haiku-4.5",
					Model:           "claude-haiku-4-5-20251001",
					Efforts:         []string{EffortMedium},
					ThinkingModes:   []string{"default"},
					MaxOutputTokens: 16000,
					SupportsTools:   &testBoolTrue,
					Contexts:        []config.AdapterModelContext{{Tokens: 200000}},
				}
			},
			wantSub: "supports_vision",
		},
		{
			name: "invalid_logprobs_value_rejected",
			mutate: func(cfg *config.AdapterConfig) {
				cfg.Logprobs = config.AdapterLogprobs{
					Anthropic: "verbatim",
				}
			},
			wantSub: "invalid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			tc.mutate(&cfg)
			_, err := NewRegistry(cfg)
			if err == nil {
				t.Fatalf("want error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v; want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestNewRegistryHappyPathCapabilitiesPropagated(t *testing.T) {
	cfg := baseConfig()
	cfg.Families["haiku-4-5"] = config.AdapterFamily{
		AliasPrefix:     "haiku-4.5",
		Model:           "claude-haiku-4-5-20251001",
		Efforts:         []string{EffortMedium},
		ThinkingModes:   []string{"default"},
		MaxOutputTokens: 16000,
		SupportsTools:   &testBoolTrue,
		SupportsVision:  &testBoolFalse,
		Contexts:        []config.AdapterModelContext{{Tokens: 200000}},
	}
	r, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	found := false
	for alias, m := range r.Models() {
		if m.FamilySlug != "haiku-4-5" {
			continue
		}
		found = true
		if !m.SupportsTools || m.SupportsVision {
			t.Fatalf("alias %q: SupportsTools=%v SupportsVision=%v; want true,false",
				alias, m.SupportsTools, m.SupportsVision)
		}
	}
	if !found {
		t.Fatal("no resolved models for family haiku-4-5")
	}
}

func TestNewRegistryLogprobsDropAccepted(t *testing.T) {
	cfg := baseConfig()
	cfg.Logprobs = config.AdapterLogprobs{
		Anthropic: "drop",
	}
	if _, err := NewRegistry(cfg); err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
}
