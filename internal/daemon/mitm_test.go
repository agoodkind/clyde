package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
)

func TestProviderLaunchEnvironmentReturnsClaudeMITMEnvWhenEnabled(t *testing.T) {
	writeMITMConfig(t, true)
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	caDir := t.TempDir()
	mitmCfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: filepath.Join(caDir, "clyde-mitm-ca.crt"),
			KeyPath:  filepath.Join(caDir, "clyde-mitm-ca.key"),
		},
	}
	proxy, err := mitm.NewProxy(mitmCfg, slog.New(slog.NewTextHandler(io.Discard, nil)), listener)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv.SetMITMProxyAccessor(func() *mitm.Proxy { return proxy })

	resp, err := srv.ProviderLaunchEnvironment(context.Background(), &clydev1.ProviderLaunchEnvironmentRequest{
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("ProviderLaunchEnvironment returned error: %v", err)
	}
	baseURL, ok := launchEnvironmentValue(resp.GetEnvironment(), "ANTHROPIC_BASE_URL")
	if !ok {
		t.Fatalf("ANTHROPIC_BASE_URL missing from response: %#v", resp.GetEnvironment())
	}
	if !strings.HasPrefix(baseURL, "http://[::1]:") {
		t.Fatalf("ANTHROPIC_BASE_URL=%q, want daemon MITM base URL", baseURL)
	}
}

func TestAdapterMITMOverrideReturnsEmptyWhenDefaultDisabled(t *testing.T) {
	cfg := config.Config{
		MITM: config.MITMConfig{
			EnabledDefault: false,
			Providers:      config.MITMProviderSet{"claude"},
		},
	}

	got := adapterMITMOverride(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	if got != "" {
		t.Fatalf("adapterMITMOverride = %q, want empty override", got)
	}
}

func writeMITMConfig(t *testing.T, enabledDefault bool) {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfgDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	enabled := "false"
	if enabledDefault {
		enabled = "true"
	}
	cfg := []byte("[mitm]\nenabled_default = " + enabled + "\nproviders = [\"claude\"]\nbody_mode = \"summary\"\ncapture_dir = \"" + t.TempDir() + "\"\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), cfg, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func launchEnvironmentValue(environment []*clydev1.EnvironmentVariable, key string) (string, bool) {
	for _, item := range environment {
		if item.GetKey() == key {
			return item.GetValue(), true
		}
	}
	return "", false
}
