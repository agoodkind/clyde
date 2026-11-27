package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAntigravityOAuthClientSecretSources(t *testing.T) {
	t.Setenv("CLYDE_TEST_ANTIGRAVITY_SECRET_SOURCE", "fixture-secret")

	const section = "[adapter.antigravity]\nenabled = true\n"
	const clientID = "client_id = \"fixture-client\"\n"
	const literal = "client_secret = \"fixture-secret\"\n"
	const environment = "client_secret_env = \"CLYDE_TEST_ANTIGRAVITY_SECRET_SOURCE\"\n"
	const file = "client_secret_file = \"client-secret.txt\"\n"

	cases := []struct {
		name, settings string
		wantErr        error
	}{
		{name: "missing client ID", settings: environment, wantErr: errAntigravityClientID},
		{name: "missing secret", settings: clientID, wantErr: errEmptySecretReference},
		{name: "literal", settings: clientID + literal},
		{name: "environment", settings: clientID + environment},
		{name: "relative file", settings: clientID + file},
		{
			name:     "literal and environment",
			settings: clientID + literal + environment,
			wantErr:  errSecretSourceConflict,
		},
		{
			name:     "literal and file",
			settings: clientID + literal + file,
			wantErr:  errSecretSourceConflict,
		},
		{
			name:     "environment and file",
			settings: clientID + environment + file,
			wantErr:  errSecretSourceConflict,
		},
	}

	for _, test := range cases {
		t.Run(
			test.name,
			func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "client-secret.txt")
				if err := os.WriteFile(path, []byte("fixture-secret\n"), 0o600); err != nil {
					t.Fatal(err)
				}

				writeModelConfig(t, dir, section+test.settings)
				cfg, err := loadConfig(dir)
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("config error = %v, want %v", err, test.wantErr)
				}

				if test.wantErr != nil {
					return
				}

				id, secret, err := cfg.Adapter.Antigravity.ResolveOAuthClient()
				if err != nil || id != "fixture-client" || secret != "fixture-secret" {
					t.Fatal("configuration did not resolve the fixture credentials")
				}
			},
		)
	}
}

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
