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

func TestProviderLaunchEnvironmentReturnsCodexMITMEnvWhenEnabled(t *testing.T) {
	caDir := t.TempDir()
	certPath := filepath.Join(caDir, "clyde-mitm-ca.crt")
	keyPath := filepath.Join(caDir, "clyde-mitm-ca.key")
	writeMITMConfigWithCA(t, true, certPath, keyPath)
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	mitmCfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: certPath,
			KeyPath:  keyPath,
		},
	}
	proxy, err := mitm.NewProxy(mitmCfg, slog.New(slog.NewTextHandler(io.Discard, nil)), listener)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv.SetMITMProxyAccessor(func() *mitm.Proxy { return proxy })

	resp, err := srv.ProviderLaunchEnvironment(context.Background(), &clydev1.ProviderLaunchEnvironmentRequest{
		Provider: "codex",
	})
	if err != nil {
		t.Fatalf("ProviderLaunchEnvironment returned error: %v", err)
	}
	baseURL, ok := launchEnvironmentValue(resp.GetEnvironment(), "OPENAI_BASE_URL")
	if !ok {
		t.Fatalf("OPENAI_BASE_URL missing from response: %#v", resp.GetEnvironment())
	}
	if baseURL != "http://[::1]:11434/v1" {
		t.Fatalf("OPENAI_BASE_URL=%q, want adapter base URL", baseURL)
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		value, ok := launchEnvironmentValue(resp.GetEnvironment(), key)
		if !ok {
			t.Fatalf("%s missing from response: %#v", key, resp.GetEnvironment())
		}
		if !strings.HasPrefix(value, "http://[::1]:") {
			t.Fatalf("%s=%q, want daemon MITM proxy URL", key, value)
		}
	}
	for _, key := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		value, ok := launchEnvironmentValue(resp.GetEnvironment(), key)
		if !ok {
			t.Fatalf("%s missing from response: %#v", key, resp.GetEnvironment())
		}
		if value != certPath {
			t.Fatalf("%s=%q, want %q", key, value, certPath)
		}
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
	writeMITMConfigWithCA(t, enabledDefault, "", "")
}

func writeMITMConfigWithCA(t *testing.T, enabledDefault bool, certPath, keyPath string) {
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
	cfgText := "[mitm]\nenabled_default = " + enabled + "\nproviders = [\"claude\", \"codex\"]\nbody_mode = \"summary\"\ncapture_dir = \"" + t.TempDir() + "\"\n"
	if certPath != "" {
		cfgText += "\n[mitm.ca]\ncert_path = \"" + certPath + "\"\nkey_path = \"" + keyPath + "\"\n"
	}
	cfg := []byte(cfgText)
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
