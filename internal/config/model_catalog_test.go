package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadModelCatalog(t *testing.T) {
	dir := t.TempDir()
	prompt := []byte("exact prompt bytes\x00\n")
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatalf("create prompts directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "gpt.md"), prompt, 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	body := `[adapter]
default_model = "gpt-test"

[adapter.codex]
enabled = true

[adapter.model_profiles.codex-standard]
contexts = [{ name = "standard", tokens = 372000, transport_limits = [{ transport = "codex_http", tokens = 272000 }, { transport = "codex_websocket", tokens = 372000 }] }]
max_output_tokens = 128000
reasoning_efforts = ["low", "medium", "high", "ultra"]
reasoning_effort_wire_values = { ultra = "max" }
default_effort = "medium"
supports_tools = true
supports_vision = true

[adapter.model_profiles.codex-standard.thinking_profiles.default]
mode = "enabled"
budget_tokens = 1000

[adapter.models."gpt-test"]
provider = "codex"
wire_model = "gpt-test-wire"
profile = "codex-standard"
instructions_file = "prompts/gpt.md"
advertise = true
aliases = [{ id = "gpt-test-medium", advertise = true, reasoning_effort = "medium", context = "standard", thinking_profile = "default" }]
pricing = { input_per_mtok = 2.5, output_per_mtok = 15 }
generated_aliases = { prefix = "clyde-gpt-test", advertise = false, dimensions = ["context", "reasoning_effort"] }

[[adapter.model_routes]]
match = "gpt-*"
surfaces = ["cursor", "openai"]
provider = "codex"
wire_model_policy = "preserve"
capability_policy = "passthrough"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	profile := cfg.Adapter.ModelProfiles["codex-standard"]
	if profile.DefaultEffort != AdapterReasoningEffort("medium") {
		t.Fatalf("default effort = %q, want medium", profile.DefaultEffort)
	}
	if profile.ReasoningEffortWireValues[AdapterReasoningEffort("ultra")] != AdapterReasoningEffort("max") {
		t.Fatalf("reasoning effort wire values = %#v, want ultra mapped to max", profile.ReasoningEffortWireValues)
	}
	if len(profile.Contexts) != 1 || len(profile.Contexts[0].TransportLimits) != 2 {
		t.Fatalf("contexts = %#v", profile.Contexts)
	}
	if profile.ThinkingProfiles["default"].Mode != AdapterThinkingModeEnabled {
		t.Fatalf("thinking profiles = %#v", profile.ThinkingProfiles)
	}
	model := cfg.Adapter.Models["gpt-test"]
	if model.Provider != AdapterModelProviderCodex {
		t.Fatalf("provider = %q, want codex", model.Provider)
	}
	if model.Instructions != string(prompt) {
		t.Fatalf("instructions bytes = %q, want %q", model.Instructions, prompt)
	}
	if len(model.Aliases) != 1 || model.Aliases[0].ThinkingProfile != "default" {
		t.Fatalf("aliases = %#v", model.Aliases)
	}
	if model.Pricing.InputPerMTok != 2.5 || model.GeneratedAliases.Prefix != "clyde-gpt-test" {
		t.Fatalf("model pricing/generated aliases = %#v", model)
	}
	if len(cfg.Adapter.ModelRoutes) != 1 {
		t.Fatalf("routes = %#v", cfg.Adapter.ModelRoutes)
	}
}

func TestLoadModelCatalogIgnoresUnknownAndLegacyFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown nested field", body: "[adapter.codex]\nenabled = true\nfuture_toggle = true\n"},
		{name: "legacy families", body: "[adapter.families.old]\nmodel = \"claude-old\"\n"},
		{name: "legacy codex models", body: "[adapter.codex]\nmodels = []\n"},
		{name: "legacy global pricing", body: "[adapter.pricing.old]\ninput_per_mtok = 1\n"},
		{name: "legacy model shape", body: "[adapter.models.old]\nbackend = \"claude\"\nmodel = \"claude-old\"\n"},
		{
			name: "complete legacy model shape",
			body: "[adapter.models.old]\n" +
				"backend = \"passthrough_override\"\n" +
				"model = \"legacy-wire-model\"\n" +
				"context = 200000\n" +
				"observed_context = 180000\n" +
				"efforts = [\"low\", \"high\"]\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeModelConfig(t, dir, test.body)
			cfg, err := loadConfig(dir)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if len(cfg.Adapter.Models) != 0 {
				t.Fatalf("ignored input created model declarations: %#v", cfg.Adapter.Models)
			}
		})
	}
}

func TestLoadModelCatalogRejectsPartialCurrentModelDeclarations(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "alias", body: "aliases = [{ id = \"gpt-alias\" }]\n"},
		{name: "advertise", body: "advertise = true\n"},
		{name: "instructions file", body: "instructions_file = \"prompts/gpt.md\"\n"},
		{name: "pricing", body: "pricing = { input_per_mtok = 1 }\n"},
		{name: "generated aliases", body: "generated_aliases = { prefix = \"clyde-gpt\", dimensions = [\"context\"] }\n"},
		{name: "passthrough override", body: "passthrough_override = \"legacy\"\n"},
		{name: "wire profile", body: "wire_profile = \"claude-default\"\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if strings.Contains(test.body, "instructions_file") {
				if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
					t.Fatalf("create prompts directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "prompts", "gpt.md"), []byte("prompt"), 0o644); err != nil {
					t.Fatalf("write prompt: %v", err)
				}
			}
			body := validProfileTOML() + "[adapter.models.partial]\n" + test.body
			writeModelConfig(t, dir, body)
			_, err := loadConfig(dir)
			if err == nil || !strings.Contains(err.Error(), "adapter.models.partial.profile") {
				t.Fatalf("error = %v, want partial model profile validation", err)
			}
		})
	}
}

func TestLoadModelCatalogRejectsConflictingPricingForNormalizedWireModel(t *testing.T) {
	body := validProfileTOML() + "[adapter.codex]\nenabled = true\n" +
		"[adapter.models.zeta]\nprovider = \"codex\"\nwire_model = \" GPT-Wire \"\nprofile = \"test\"\npricing = { input_per_mtok = 2.5, output_per_mtok = 15 }\n" +
		"[adapter.models.alpha]\nprovider = \"codex\"\nwire_model = \"gpt-wire\"\nprofile = \"test\"\npricing = { input_per_mtok = 1, output_per_mtok = 6 }\n" +
		"[adapter.models.beta]\nprovider = \"codex\"\nwire_model = \"gpt-wire\"\nprofile = \"test\"\npricing = { input_per_mtok = 1, output_per_mtok = 6 }\n"
	want := "adapter.models.alpha and adapter.models.zeta declare conflicting pricing for wire_model \"gpt-wire\""
	for range 20 {
		dir := t.TempDir()
		writeModelConfig(t, dir, body)
		_, err := loadConfig(dir)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want deterministic substring %q", err, want)
		}
	}
}

func TestLoadModelCatalogAllowsCompatiblePricingForNormalizedWireModel(t *testing.T) {
	tests := []struct {
		name          string
		secondPricing string
	}{
		{name: "identical pricing", secondPricing: "pricing = { input_per_mtok = 2.5, output_per_mtok = 15 }\n"},
		{name: "only one priced declaration"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			body := validProfileTOML() + "[adapter.codex]\nenabled = true\n" +
				"[adapter.models.one]\nprovider = \"codex\"\nwire_model = \" GPT-Wire \"\nprofile = \"test\"\npricing = { input_per_mtok = 2.5, output_per_mtok = 15 }\n" +
				"[adapter.models.two]\nprovider = \"codex\"\nwire_model = \"gpt-wire\"\nprofile = \"test\"\n" + test.secondPricing
			writeModelConfig(t, dir, body)
			if _, err := loadConfig(dir); err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
		})
	}
}

func TestLoadModelCatalogRejectsMalformedTOML(t *testing.T) {
	dir := t.TempDir()
	writeModelConfig(t, dir, "[adapter.models.\n")
	_, err := loadConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "failed to parse") {
		t.Fatalf("error = %v, want parse failure", err)
	}
}

func TestLoadModelCatalogValidatesProfiles(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		wantSub string
	}{
		{name: "missing contexts", profile: "max_output_tokens = 100\nsupports_tools = true\nsupports_vision = true\n", wantSub: "contexts"},
		{name: "nonpositive context", profile: "contexts = [{ name = \"standard\", tokens = 0 }]\nmax_output_tokens = 100\nsupports_tools = true\nsupports_vision = true\n", wantSub: "tokens must be positive"},
		{name: "nonpositive output", profile: "contexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 0\nsupports_tools = true\nsupports_vision = true\n", wantSub: "max_output_tokens"},
		{name: "duplicate transport", profile: "contexts = [{ name = \"standard\", tokens = 100, transport_limits = [{ transport = \"codex_http\", tokens = 90 }, { transport = \"codex_http\", tokens = 80 }] }]\nmax_output_tokens = 10\nsupports_tools = true\nsupports_vision = true\n", wantSub: "duplicate transport"},
		{name: "unsupported transport", profile: "contexts = [{ name = \"standard\", tokens = 100, transport_limits = [{ transport = \"future\", tokens = 90 }] }]\nmax_output_tokens = 10\nsupports_tools = true\nsupports_vision = true\n", wantSub: "unsupported transport"},
		{name: "duplicate effort", profile: "contexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nreasoning_efforts = [\"medium\", \"medium\"]\ndefault_effort = \"medium\"\nsupports_tools = true\nsupports_vision = true\n", wantSub: "duplicate reasoning effort"},
		{name: "empty effort", profile: "contexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nreasoning_efforts = [\"\"]\nsupports_tools = true\nsupports_vision = true\n", wantSub: "empty effort"},
		{name: "invalid default effort", profile: "contexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nreasoning_efforts = [\"medium\"]\ndefault_effort = \"high\"\nsupports_tools = true\nsupports_vision = true\n", wantSub: "default_effort"},
		{name: "wire effort key is not declared", profile: "contexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nreasoning_efforts = [\"medium\"]\nreasoning_effort_wire_values = { ultra = \"max\" }\nsupports_tools = true\nsupports_vision = true\n", wantSub: "reasoning_effort_wire_values"},
		{name: "wire effort value is empty", profile: "contexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nreasoning_efforts = [\"ultra\"]\nreasoning_effort_wire_values = { ultra = \"\" }\nsupports_tools = true\nsupports_vision = true\n", wantSub: "reasoning_effort_wire_values"},
		{name: "missing tools capability", profile: "contexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nsupports_vision = true\n", wantSub: "supports_tools"},
		{name: "invalid thinking profile", profile: "contexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nsupports_tools = true\nsupports_vision = true\n[adapter.model_profiles.test.thinking_profiles.bad]\nmode = \"enabled\"\nbudget_tokens = 0\n", wantSub: "budget_tokens"},
		{name: "thinking budget reaches output cap", profile: "contexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nsupports_tools = true\nsupports_vision = true\n[adapter.model_profiles.test.thinking_profiles.bad]\nmode = \"enabled\"\nbudget_tokens = 10\n", wantSub: "less than max_output_tokens"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeModelConfig(t, dir, "[adapter.model_profiles.test]\n"+test.profile)
			_, err := loadConfig(dir)
			if err == nil || !strings.Contains(err.Error(), test.wantSub) {
				t.Fatalf("error = %v, want substring %q", err, test.wantSub)
			}
		})
	}
}

func TestLoadModelCatalogValidatesReferencesAndCollisions(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name:    "missing profile reference",
			body:    "[adapter.codex]\nenabled = true\n[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"missing\"\n",
			wantSub: "profile",
		},
		{
			name: "duplicate alias",
			body: validProfileTOML() + "[adapter.codex]\nenabled = true\n" +
				"[adapter.models.one]\nprovider = \"codex\"\nwire_model = \"one\"\nprofile = \"test\"\naliases = [{ id = \"shared\" }]\n" +
				"[adapter.models.two]\nprovider = \"codex\"\nwire_model = \"two\"\nprofile = \"test\"\naliases = [{ id = \"shared\" }]\n",
			wantSub: "duplicate exact alias",
		},
		{
			name: "canonical id collides with alias",
			body: validProfileTOML() + "[adapter.codex]\nenabled = true\n" +
				"[adapter.models.one]\nprovider = \"codex\"\nwire_model = \"one\"\nprofile = \"test\"\naliases = [{ id = \"two\" }]\n" +
				"[adapter.models.two]\nprovider = \"codex\"\nwire_model = \"two\"\nprofile = \"test\"\n",
			wantSub: "duplicate exact alias",
		},
		{name: "unknown default", body: "[adapter]\ndefault_model = \"missing\"\n" + validProfileTOML(), wantSub: "default_model"},
		{
			name: "invalid passthrough reference",
			body: validProfileTOML() +
				"[adapter.models.local]\nprovider = \"passthrough_override\"\nwire_model = \"local\"\nprofile = \"test\"\npassthrough_override = \"missing\"\n",
			wantSub: "passthrough_override",
		},
		{
			name: "invalid alias effort reference",
			body: validProfileTOML() + "[adapter.codex]\nenabled = true\n" +
				"[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"test\"\naliases = [{ id = \"gpt-high\", reasoning_effort = \"high\" }]\n",
			wantSub: "reasoning_effort",
		},
		{
			name: "invalid alias context reference",
			body: validProfileTOML() + "[adapter.codex]\nenabled = true\n" +
				"[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"test\"\naliases = [{ id = \"gpt-large\", context = \"large\" }]\n",
			wantSub: "context",
		},
		{
			name: "invalid nonanthropic wire profile",
			body: validProfileTOML() + "[adapter.codex]\nenabled = true\n" +
				"[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"test\"\nwire_profile = \"claude-default\"\n",
			wantSub: "wire_profile",
		},
		{
			name: "negative model-local pricing",
			body: validProfileTOML() + "[adapter.codex]\nenabled = true\n" +
				"[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"test\"\npricing = { input_per_mtok = -1 }\n",
			wantSub: "pricing",
		},
		{
			name: "advertised context aliases require default",
			body: "[adapter.codex]\nenabled = true\n[adapter.model_profiles.test]\ncontexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nsupports_tools = true\nsupports_vision = true\n" +
				"[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"test\"\ngenerated_aliases = { prefix = \"clyde-gpt\", advertise = true, dimensions = [\"context\"] }\n",
			wantSub: "default_effort",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeModelConfig(t, dir, test.body)
			_, err := loadConfig(dir)
			if err == nil || !strings.Contains(err.Error(), test.wantSub) {
				t.Fatalf("error = %v, want substring %q", err, test.wantSub)
			}
		})
	}
}

func TestLoadModelCatalogAllowsEffortGeneratedAliasesWithoutDefault(t *testing.T) {
	dir := t.TempDir()
	body := "[adapter.codex]\nenabled = true\n[adapter.model_profiles.test]\ncontexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nreasoning_efforts = [\"low\", \"medium\"]\nsupports_tools = true\nsupports_vision = true\n" +
		"[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"test\"\ngenerated_aliases = { prefix = \"clyde-gpt\", advertise = true, dimensions = [\"reasoning_effort\"] }\n"
	writeModelConfig(t, dir, body)
	if _, err := loadConfig(dir); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
}

func TestLoadModelCatalogValidatesProvidersAndRoutes(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantSub string
	}{
		{name: "advertised disabled provider model", body: validProfileTOML() + "[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"test\"\nadvertise = true\n", wantSub: "disabled provider"},
		{name: "advertised generated alias on disabled provider", body: validProfileTOML() + "[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"test\"\ngenerated_aliases = { prefix = \"clyde-gpt\", advertise = true, dimensions = [\"context\"] }\n", wantSub: "disabled provider"},
		{name: "default disabled provider model", body: "[adapter]\ndefault_model = \"gpt\"\n" + validProfileTOML() + "[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"test\"\nadvertise = false\n", wantSub: "default_model"},
		{name: "invalid glob", body: routeTOML("gpt-[", "cursor", true), wantSub: "invalid glob"},
		{name: "empty surfaces", body: "[adapter.codex]\nenabled = true\n[[adapter.model_routes]]\nmatch = \"gpt-*\"\nsurfaces = []\nprovider = \"codex\"\nwire_model_policy = \"preserve\"\ncapability_policy = \"passthrough\"\n", wantSub: "surfaces"},
		{name: "duplicate surfaces", body: "[adapter.codex]\nenabled = true\n[[adapter.model_routes]]\nmatch = \"gpt-*\"\nsurfaces = [\"cursor\", \"cursor\"]\nprovider = \"codex\"\nwire_model_policy = \"preserve\"\ncapability_policy = \"passthrough\"\n", wantSub: "duplicate surface"},
		{name: "disabled route provider", body: routeTOML("gpt-*", "openai", false), wantSub: "disabled provider"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeModelConfig(t, dir, test.body)
			_, err := loadConfig(dir)
			if err == nil || !strings.Contains(err.Error(), test.wantSub) {
				t.Fatalf("error = %v, want substring %q", err, test.wantSub)
			}
		})
	}
}

func TestLoadModelCatalogAllowsHiddenDisabledProviderModel(t *testing.T) {
	dir := t.TempDir()
	writeModelConfig(t, dir, validProfileTOML()+"[adapter.models.gpt]\nprovider = \"codex\"\nwire_model = \"gpt\"\nprofile = \"test\"\nadvertise = false\n")
	if _, err := loadConfig(dir); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
}

func TestAdapterProviderEnabledTreatsDirectOAuthAsAnthropic(t *testing.T) {
	adapter := AdapterConfig{DirectOAuth: true}
	if !adapterProviderEnabled(adapter, AdapterModelProviderAnthropic) {
		t.Fatal("direct_oauth should enable the anthropic provider")
	}
}

func validProfileTOML() string {
	return "[adapter.model_profiles.test]\ncontexts = [{ name = \"standard\", tokens = 100 }]\nmax_output_tokens = 10\nreasoning_efforts = [\"medium\"]\ndefault_effort = \"medium\"\nsupports_tools = true\nsupports_vision = true\n\n"
}

func routeTOML(glob string, surface string, enabled bool) string {
	enabledLine := ""
	if enabled {
		enabledLine = "[adapter.codex]\nenabled = true\n"
	}
	return enabledLine + "[[adapter.model_routes]]\nmatch = \"" + glob + "\"\nsurfaces = [\"" + surface + "\"]\nprovider = \"codex\"\nwire_model_policy = \"preserve\"\ncapability_policy = \"passthrough\"\n"
}

func writeModelConfig(t *testing.T, dir string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
