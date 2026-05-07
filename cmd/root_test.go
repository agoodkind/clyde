package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/ui"
)

func TestSessionEventFromProtoMapsBinaryUpdate(t *testing.T) {
	ev := sessionEventFromProto(&clydev1.SubscribeRegistryResponse{
		Kind:         clydev1.SubscribeRegistryResponse_KIND_CLYDE_BINARY_UPDATED,
		BinaryPath:   "/tmp/clyde",
		BinaryReason: "mtime_changed",
		BinaryHash:   "abc123",
	})

	if ev.Kind != "CLYDE_BINARY_UPDATED" {
		t.Fatalf("kind=%q want CLYDE_BINARY_UPDATED", ev.Kind)
	}
	if ev.BinaryPath != "/tmp/clyde" {
		t.Fatalf("binary path=%q want /tmp/clyde", ev.BinaryPath)
	}
	if ev.BinaryReason != "mtime_changed" {
		t.Fatalf("binary reason=%q want mtime_changed", ev.BinaryReason)
	}
	if ev.BinaryHash != "abc123" {
		t.Fatalf("binary hash=%q want abc123", ev.BinaryHash)
	}
}

func TestConsumeTUIReturnSessionRestoresAndClearsEnv(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := session.NewFileStore(config.GlobalDataDir())
	want := session.NewSession("chat-one", "session-uuid")
	if err := store.Create(want); err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Setenv(ui.EnvTUIReturnSessionID, "session-uuid")
	t.Setenv(ui.EnvTUIReturnSessionName, "chat-one")

	got := consumeTUIReturnSession()

	if got == nil {
		t.Fatalf("consumeTUIReturnSession returned nil")
	}
	if got.Name != "chat-one" || got.Metadata.ProviderSessionID() != "session-uuid" {
		t.Fatalf("restored session = %s/%s, want chat-one/session-uuid", got.Name, got.Metadata.ProviderSessionID())
	}
	if value := os.Getenv(ui.EnvTUIReturnSessionID); value != "" {
		t.Fatalf("%s still set to %q", ui.EnvTUIReturnSessionID, value)
	}
	if value := os.Getenv(ui.EnvTUIReturnSessionName); value != "" {
		t.Fatalf("%s still set to %q", ui.EnvTUIReturnSessionName, value)
	}
}

func TestApplyClaudeMITMEnvAddsAnthropicBaseURLForPassthrough(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cfgDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := []byte("[mitm]\nenabled_default = true\nproviders = [\"claude\"]\nbody_mode = \"summary\"\ncapture_dir = \"" + t.TempDir() + "\"\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), cfg, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got := applyClaudeMITMEnv(context.Background(), []string{"ANTHROPIC_BASE_URL=https://old.example", "KEEP=1"})

	if !envContains(got, "KEEP", "1") {
		t.Fatalf("KEEP env missing: %v", got)
	}
	baseURL, ok := envValue(got, "ANTHROPIC_BASE_URL")
	if !ok {
		t.Fatalf("ANTHROPIC_BASE_URL missing: %v", got)
	}
	if !strings.HasPrefix(baseURL, "http://[::1]:") {
		t.Fatalf("ANTHROPIC_BASE_URL=%q, want local MITM proxy", baseURL)
	}
}

func envContains(env []string, key, want string) bool {
	got, ok := envValue(env, key)
	return ok && got == want
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, prefix); ok {
			return value, true
		}
	}
	return "", false
}
