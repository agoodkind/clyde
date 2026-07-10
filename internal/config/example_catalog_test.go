package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleUsesDeclarativeModelCatalog(t *testing.T) {
	examplePath := filepath.Join("..", "..", "clyde.example.toml")
	example, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}

	activeLines := make([]string, 0)
	for _, line := range strings.Split(string(example), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		activeLines = append(activeLines, trimmed)
	}
	active := strings.Join(activeLines, "\n")
	for _, legacy := range []string{
		"[adapter.families.",
		"[[adapter.codex.models]]",
		"[adapter.pricing.",
		"native_model_routing =",
	} {
		if strings.Contains(active, legacy) {
			t.Fatalf("example contains active legacy declaration %q", legacy)
		}
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), example, 0o600); err != nil {
		t.Fatalf("write example config: %v", err)
	}
	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	if len(cfg.Adapter.ModelProfiles) == 0 || len(cfg.Adapter.Models) == 0 {
		t.Fatalf("declarative catalog is empty: profiles=%d models=%d", len(cfg.Adapter.ModelProfiles), len(cfg.Adapter.Models))
	}
	if len(cfg.Adapter.ModelRoutes) != 2 {
		t.Fatalf("model routes = %#v, want two ordered provider claims", cfg.Adapter.ModelRoutes)
	}
	gptRoute := cfg.Adapter.ModelRoutes[0]
	if gptRoute.Match != "gpt-*" || gptRoute.Provider != AdapterModelProviderCodex ||
		!sameIngressSurfaces(gptRoute.Surfaces, []AdapterIngressSurface{AdapterIngressCursor, AdapterIngressOpenAI}) {
		t.Fatalf("first route = %#v, want gpt-* on cursor and openai to codex", gptRoute)
	}
	claudeRoute := cfg.Adapter.ModelRoutes[1]
	if claudeRoute.Match != "claude-*" || claudeRoute.Provider != AdapterModelProviderAnthropic ||
		!sameIngressSurfaces(claudeRoute.Surfaces, []AdapterIngressSurface{AdapterIngressCursor, AdapterIngressOpenAI, AdapterIngressAnthropic}) {
		t.Fatalf("second route = %#v, want claude-* on all supported surfaces to anthropic", claudeRoute)
	}
	for modelID, declaration := range cfg.Adapter.Models {
		if !declaration.Advertise {
			continue
		}
		if strings.TrimSpace(declaration.Profile) == "" {
			t.Fatalf("advertised model %q has no profile", modelID)
		}
		if _, ok := cfg.Adapter.ModelProfiles[declaration.Profile]; !ok {
			t.Fatalf("advertised model %q references missing profile %q", modelID, declaration.Profile)
		}
	}
}

func sameIngressSurfaces(got []AdapterIngressSurface, want []AdapterIngressSurface) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
