package mitm

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func TestPrepareLaunchReturnsProxyEnvArgsAndCaptureDir(t *testing.T) {
	restore := stubPrepareLaunchDependencies(t, config.MITMConfig{
		CaptureDir:     "/tmp/original-captures",
		EnabledDefault: false,
		Providers:      config.MITMProviderSet{"claude"},
	})
	defer restore()

	configuredProxyURL := ""
	prepared, err := PrepareLaunch(context.Background(), PrepareLaunchOptions{
		Profile: LaunchProfile{
			Name:         "test-electron",
			BinaryFinder: func() (string, error) { return "/tmp/Test Electron", nil },
			IsElectron:   true,
			Configurator: func(proxyURL string, _ LaunchProfileOptions) error {
				configuredProxyURL = proxyURL
				return nil
			},
		},
		CaptureDir: "/tmp/override-captures",
		Force:      true,
		ExtraArgs:  []string{"--extra"},
	})
	if err != nil {
		t.Fatalf("PrepareLaunch: %v", err)
	}
	if prepared.Binary != "/tmp/Test Electron" {
		t.Fatalf("Binary = %q, want /tmp/Test Electron", prepared.Binary)
	}
	if prepared.ProxyURL != "http://[::1]:48888" {
		t.Fatalf("ProxyURL = %q, want http://[::1]:48888", prepared.ProxyURL)
	}
	if prepared.CaptureDir != "/tmp/override-captures" {
		t.Fatalf("CaptureDir = %q, want /tmp/override-captures", prepared.CaptureDir)
	}
	assertContains(t, prepared.Args, "--proxy-server=http://[::1]:48888")
	assertContains(t, prepared.Args, "--ignore-certificate-errors")
	assertContains(t, prepared.Args, "--extra")
	assertContains(t, prepared.Env, "HTTPS_PROXY=http://[::1]:48888")
	assertContains(t, prepared.Env, "HTTP_PROXY=http://[::1]:48888")
	assertContains(t, prepared.Env, "ALL_PROXY=http://[::1]:48888")
	if slices.Contains(prepared.Env, "HTTPS_PROXY=http://old-proxy") {
		t.Fatalf("Env kept old HTTPS_PROXY: %v", prepared.Env)
	}
	if configuredProxyURL != "http://[::1]:48888" {
		t.Fatalf("configuredProxyURL = %q, want http://[::1]:48888", configuredProxyURL)
	}
}

func TestPrepareLaunchAppliesProfileValidation(t *testing.T) {
	restore := stubPrepareLaunchDependencies(t, config.MITMConfig{
		CaptureDir:     "/tmp/captures",
		EnabledDefault: true,
		Providers:      config.MITMProviderSet{"all"},
	})
	defer restore()

	gotOptions := LaunchProfileOptions{}
	_, err := PrepareLaunch(context.Background(), PrepareLaunchOptions{
		Profile: LaunchProfile{
			Name:         "test-profile",
			BinaryFinder: func() (string, error) { return "/tmp/test-profile", nil },
			Preflight: func(opts LaunchProfileOptions) error {
				gotOptions = opts
				return nil
			},
		},
		ProfileOptions: LaunchProfileOptions{Isolated: true, IsolatedProfileDir: "/tmp/profile"},
		Force:          true,
	})
	if err != nil {
		t.Fatalf("PrepareLaunch: %v", err)
	}
	if !gotOptions.Isolated {
		t.Fatal("Isolated = false, want true")
	}
	if gotOptions.IsolatedProfileDir != "/tmp/profile" {
		t.Fatalf("IsolatedProfileDir = %q, want /tmp/profile", gotOptions.IsolatedProfileDir)
	}
	if !gotOptions.Force {
		t.Fatal("Force = false, want true")
	}
}

func stubPrepareLaunchDependencies(t *testing.T, mitmConfig config.MITMConfig) func() {
	t.Helper()
	originalLoad := loadGlobalConfigForLaunch
	originalEnsure := ensureProxyForLaunch
	originalEnv := parentEnvForLaunch
	loadGlobalConfigForLaunch = func() (*config.Config, error) {
		return &config.Config{MITM: mitmConfig}, nil
	}
	ensureProxyForLaunch = func(cfg config.MITMConfig, _ *slog.Logger) (*Proxy, error) {
		if !cfg.EnabledDefault {
			t.Fatalf("EnabledDefault = false, want true")
		}
		if len(cfg.Providers) != 1 || cfg.Providers[0] != "all" {
			t.Fatalf("Providers = %v, want [all]", cfg.Providers)
		}
		return &Proxy{base: "http://[::1]:48888"}, nil
	}
	parentEnvForLaunch = func() []string {
		return []string{"PATH=/usr/bin", "HTTPS_PROXY=http://old-proxy"}
	}
	return func() {
		loadGlobalConfigForLaunch = originalLoad
		ensureProxyForLaunch = originalEnsure
		parentEnvForLaunch = originalEnv
	}
}

func assertContains(t *testing.T, items []string, want string) {
	t.Helper()
	if !slices.Contains(items, want) {
		t.Fatalf("%q not found in %v", want, items)
	}
}
