package config

import (
	"strings"
	"testing"
)

func TestLoadAntigravityRouteRequiresEnabledBackend(t *testing.T) {
	t.Setenv("CLYDE_TEST_ANTIGRAVITY_CLIENT_SECRET", "fixture-secret")

	const route = `
[[adapter.model_routes]]
match = "gemini-*"
surfaces = ["cursor", "openai"]
provider = "antigravity"
wire_model_policy = "preserve"
capability_policy = "passthrough"
`
	dir := t.TempDir()
	writeModelConfig(t, dir, route)
	if _, err := loadConfig(dir); err == nil ||
		!strings.Contains(err.Error(), `selects disabled provider "antigravity"`) {
		t.Fatalf("loadConfig without [adapter.antigravity] = %v, want disabled provider error", err)
	}

	const backend = `
[adapter.antigravity]
enabled = true
client_id = "fixture-client"
client_secret_env = "CLYDE_TEST_ANTIGRAVITY_CLIENT_SECRET"
credentials_file = "/example/token"
project = "example-project"
client_version = "1.2.3"

[adapter.antigravity.reasoning]
include_thoughts = true
`
	writeModelConfig(t, dir, backend+route)
	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	got := cfg.Adapter.Antigravity
	want := AdapterAntigravity{
		Enabled:         true,
		BaseURL:         "",
		CredentialsFile: "/example/token",
		Project:         "example-project",
		ClientVersion:   "1.2.3",
		ClientID:        "fixture-client",
		ClientSecretEnv: "CLYDE_TEST_ANTIGRAVITY_CLIENT_SECRET",
		Reasoning:       AdapterAntigravityReasoning{IncludeThoughts: true},
	}
	if got != want {
		t.Fatalf("Antigravity config = %+v, want %+v", got, want)
	}

	clientID, secret, err := got.ResolveOAuthClient()
	if err != nil || clientID != "fixture-client" || secret != "fixture-secret" {
		t.Fatal("OAuth client did not resolve the configured ID and environment value")
	}

	if got.ResolvedBaseURL() != "https://daily-cloudcode-pa.googleapis.com" {
		t.Fatalf("Code Assist origin = %q, want default", got.ResolvedBaseURL())
	}

	if cfg.Adapter.ModelRoutes[0].Provider != AdapterModelProviderAntigravity {
		t.Fatalf("route provider = %q, want antigravity", cfg.Adapter.ModelRoutes[0].Provider)
	}
}

func TestAntigravityWildcardMustRestrictModelsToGemini(t *testing.T) {
	t.Setenv("CLYDE_TEST_ANTIGRAVITY_CLIENT_SECRET", "fixture-secret")

	const route = `
[adapter.antigravity]
enabled = true
client_id = "fixture-client"
client_secret_env = "CLYDE_TEST_ANTIGRAVITY_CLIENT_SECRET"

[[adapter.model_routes]]
match = "*"
surfaces = ["cursor", "openai"]
provider = "antigravity"
wire_model_policy = "preserve"
capability_policy = "passthrough"
`
	dir := t.TempDir()
	writeModelConfig(t, dir, route)
	if _, err := loadConfig(dir); err == nil ||
		!strings.Contains(err.Error(), "match must start with gemini-") {
		t.Fatalf(
			"loadConfig with unrestricted Antigravity route = %v, want Gemini prefix error",
			err,
		)
	}
}
