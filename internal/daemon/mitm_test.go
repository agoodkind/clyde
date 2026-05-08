package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
)

func TestPrepareMITMLaunchReturnsPreparedPlanWithoutLaunching(t *testing.T) {
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	originalPrepare := prepareMITMLaunch
	originalLaunch := launchMITMUpstream
	defer func() {
		prepareMITMLaunch = originalPrepare
		launchMITMUpstream = originalLaunch
	}()

	prepareCalled := false
	prepareMITMLaunch = func(_ context.Context, opts mitm.PrepareLaunchOptions) (mitm.PreparedLaunch, error) {
		prepareCalled = true
		if opts.Profile.Name != "codex-desktop" {
			t.Fatalf("Profile.Name = %q, want codex-desktop", opts.Profile.Name)
		}
		if opts.CaptureDir != "/tmp/captures" {
			t.Fatalf("CaptureDir = %q, want /tmp/captures", opts.CaptureDir)
		}
		if !opts.Force {
			t.Fatal("Force = false, want true")
		}
		if len(opts.ExtraArgs) != 1 || opts.ExtraArgs[0] != "--probe" {
			t.Fatalf("ExtraArgs = %v, want [--probe]", opts.ExtraArgs)
		}
		return mitm.PreparedLaunch{
			ProfileName: "codex-desktop",
			Binary:      "/Applications/Codex.app/Contents/MacOS/Codex",
			Args:        []string{"--proxy-server=http://[::1]:49999", "--probe"},
			Env:         []string{"HTTPS_PROXY=http://[::1]:49999"},
			ProxyURL:    "http://[::1]:49999",
			CaptureDir:  "/tmp/captures",
		}, nil
	}
	launchMITMUpstream = func(context.Context, mitm.LaunchUpstreamOptions) error {
		return errors.New("launch should not be called")
	}

	resp, err := srv.PrepareMITMLaunch(context.Background(), &clydev1.PrepareMITMLaunchRequest{
		Upstream:   "codex-desktop",
		CaptureDir: "/tmp/captures",
		Force:      true,
		Args:       []string{"--probe"},
	})
	if err != nil {
		t.Fatalf("PrepareMITMLaunch: %v", err)
	}
	if !prepareCalled {
		t.Fatal("prepareMITMLaunch was not called")
	}
	if resp.GetUpstream() != "codex-desktop" {
		t.Fatalf("Upstream = %q, want codex-desktop", resp.GetUpstream())
	}
	if resp.GetProfileMode() != "normal" {
		t.Fatalf("ProfileMode = %q, want normal", resp.GetProfileMode())
	}
	if resp.GetCaptureDir() != "/tmp/captures" {
		t.Fatalf("CaptureDir = %q, want /tmp/captures", resp.GetCaptureDir())
	}
	if resp.GetBinary() != "/Applications/Codex.app/Contents/MacOS/Codex" {
		t.Fatalf("Binary = %q, want Codex binary", resp.GetBinary())
	}
	assertStringSlicesEqual(t, resp.GetArgs(), []string{"--proxy-server=http://[::1]:49999", "--probe"})
	assertStringSlicesEqual(t, resp.GetEnv(), []string{"HTTPS_PROXY=http://[::1]:49999"})
}

func TestPrepareMITMLaunchRejectsInvalidProfileMode(t *testing.T) {
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := srv.PrepareMITMLaunch(context.Background(), &clydev1.PrepareMITMLaunchRequest{
		Upstream:    "cursor",
		ProfileMode: "separate",
		CaptureDir:  filepath.Join(t.TempDir(), "captures"),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "profile_mode") {
		t.Fatalf("error = %q, want profile_mode", got)
	}
}

func assertStringSlicesEqual(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d: got %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q: got %v", i, got[i], want[i], got)
		}
	}
}

func TestProviderLaunchEnvironmentReturnsClaudeMITMEnvWhenEnabled(t *testing.T) {
	writeMITMConfig(t, true)
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

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

func TestAdapterMITMOverrideDoesNotStartWhenDefaultDisabled(t *testing.T) {
	oldEnsure := ensureAdapterMITMStarted
	t.Cleanup(func() {
		ensureAdapterMITMStarted = oldEnsure
	})
	called := false
	ensureAdapterMITMStarted = func(config.MITMConfig, *slog.Logger) (*mitm.Proxy, error) {
		called = true
		return nil, nil
	}
	cfg := config.Config{
		MITM: config.MITMConfig{
			EnabledDefault: false,
			Providers:      config.MITMProviderSet{"claude"},
		},
	}

	got := adapterMITMOverride(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got != "" {
		t.Fatalf("adapterMITMOverride = %q, want empty override", got)
	}
	if called {
		t.Fatalf("adapterMITMOverride started MITM with enabled_default=false")
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
